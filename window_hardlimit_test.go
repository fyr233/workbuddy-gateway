package main

import (
	"testing"
	"time"
)

// 软线是实测硬墙的 95%，硬限必须反推回实测值：
// CN 200M、INTL 100M。若硬限算错，排序时会误判账号的紧张程度。
func TestHardLimitIsMeasuredWall(t *testing.T) {
	cn := &Account{Path: "a.json", Auth: &StoredAuth{Edition: "cn"}}
	if got := accountTokenHardLimit(cn); got != 200_000_000 {
		t.Fatalf("CN 硬限应反推为 200M，实际 %d", got)
	}
	intl := &Account{Path: "b.json", Auth: &StoredAuth{Edition: "intl"}}
	if got := accountTokenHardLimit(intl); got != 100_000_000 {
		t.Fatalf("INTL 硬限应反推为 100M，实际 %d", got)
	}
	paid := &Account{Path: "p.json", IsPaidUser: true, Auth: &StoredAuth{Edition: "cn"}}
	if got := accountTokenHardLimit(paid); got != 0 {
		t.Fatalf("付费账号不纳入软限，硬限应为 0，实际 %d", got)
	}
}

// 硬限判定必须是「>= 实测墙」，而不是「>= 软线」：
// 软线之上还有 5% 缓冲，越软线不等于已过硬墙。
func TestWindowHardExceededBoundary(t *testing.T) {
	const path = "hard.json"
	resetWindowForTest(t, path)
	defer resetWindowForTest(t, path)

	hour := time.Now().Truncate(time.Hour).Unix()
	acc := &Account{Path: path, Auth: &StoredAuth{Edition: "cn"}}

	recordTokensAt(acc, testModel, hour, 190_000_000)
	if !windowOverLimit(acc, testModel) {
		t.Fatal("190M 应判定越软线")
	}
	if windowHardExceeded(acc, testModel) {
		t.Fatal("190M 未到硬墙，不应判定过硬墙")
	}

	recordTokensAt(acc, testModel, hour, 10_000_000)
	if !windowHardExceeded(acc, testModel) {
		t.Fatal("200M 应判定已过硬墙")
	}
	if windowHardExceeded(acc, "glm-x") {
		t.Fatal("其它模型不该被判定过硬墙")
	}
}

// 可用性优先：全部账号越过软线时，必须仍然选出账号，绝不返回错误。
// 即便所有账号都已过硬墙，也只能挑选、不能拒绝。
func TestNeverRejectsWhenAllOverSoftLine(t *testing.T) {
	now := time.Now()
	a := creditAccount("a.json", 1000, 100, now)
	b := creditAccount("b.json", 1000, 100, now)
	resetScheduler(t, a, b)
	defer resetScheduler(t)

	hour := now.Truncate(time.Hour).Unix()
	tokenWindowMu.Lock()
	tokenWindows["a.json"] = &accountTokenWindow{
		Path: "a.json", Edition: "cn",
		ModelBuckets: map[string][]tokenBucket{testModel: {{Hour: hour, Tokens: 260_000_000}}},
	}
	tokenWindows["b.json"] = &accountTokenWindow{
		Path: "b.json", Edition: "cn",
		ModelBuckets: map[string][]tokenBucket{testModel: {{Hour: hour, Tokens: 250_000_000}}},
	}
	tokenWindowMu.Unlock()
	defer func() {
		tokenWindowMu.Lock()
		delete(tokenWindows, "a.json")
		delete(tokenWindows, "b.json")
		tokenWindowMu.Unlock()
	}()

	for i := 0; i < 20; i++ {
		acc, _, err := nextAccountForModel(testModel, nil, "")
		if err != nil {
			t.Fatalf("全部账号越线时仍必须放行，不应报错：%v", err)
		}
		if acc == nil {
			t.Fatal("必须返回账号")
		}
	}
}

// 越过软线但未过硬墙的账号，必须优先于已过硬墙的账号被选中。
func TestPrefersAccountBelowHardWall(t *testing.T) {
	now := time.Now()
	past := creditAccount("past.json", 1000, 100, now)
	below := creditAccount("below.json", 1000, 100, now)
	resetScheduler(t, past, below)
	defer resetScheduler(t)

	hour := now.Truncate(time.Hour).Unix()
	tokenWindowMu.Lock()
	// 两个号都越软线；past 已过硬墙，below 还在缓冲区内。
	tokenWindows["past.json"] = &accountTokenWindow{
		Path: "past.json", Edition: "cn",
		ModelBuckets: map[string][]tokenBucket{testModel: {{Hour: hour, Tokens: 215_000_000}}},
	}
	tokenWindows["below.json"] = &accountTokenWindow{
		Path: "below.json", Edition: "cn",
		ModelBuckets: map[string][]tokenBucket{testModel: {{Hour: hour, Tokens: 195_000_000}}},
	}
	tokenWindowMu.Unlock()
	defer func() {
		tokenWindowMu.Lock()
		delete(tokenWindows, "past.json")
		delete(tokenWindows, "below.json")
		tokenWindowMu.Unlock()
	}()

	for i := 0; i < 10; i++ {
		acc, _, err := nextAccountForModel(testModel, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if acc.Path != "below.json" {
			t.Fatalf("应优先选未过硬墙的 below.json，实际 %s（第 %d 次）", acc.Path, i+1)
		}
	}
}

// 标准版（付费账号）不纳入软限：当所有体验版账号都越过软线时，
// 流量必须集中到标准版，而不是继续摊给已越线的号。
func TestAllFreeOverLimitRoutesToUnlimitedPaidAccount(t *testing.T) {
	now := time.Now()
	f1 := creditAccount("f1.json", 2000, 200, now)
	f1.IsPaidUser = false
	f1.PlanLabel = "体验版"
	f2 := creditAccount("f2.json", 2000, 200, now)
	f2.IsPaidUser = false
	f2.PlanLabel = "体验版"
	pro := creditAccount("pro.json", 500, 500, now)
	pro.IsPaidUser = true
	pro.PlanLabel = "标准版"
	resetScheduler(t, f1, f2, pro)
	defer resetScheduler(t)

	hour := now.Truncate(time.Hour).Unix()
	tokenWindowMu.Lock()
	for _, p := range []string{"f1.json", "f2.json"} {
		tokenWindows[p] = &accountTokenWindow{
			Path: p, Edition: "cn",
			ModelBuckets: map[string][]tokenBucket{testModel: {{Hour: hour, Tokens: 195_000_000}}},
		}
	}
	tokenWindowMu.Unlock()
	defer func() {
		tokenWindowMu.Lock()
		delete(tokenWindows, "f1.json")
		delete(tokenWindows, "f2.json")
		tokenWindowMu.Unlock()
	}()

	if accountTokenLimit(pro) != 0 {
		t.Fatalf("标准版不应纳入软限，阈值应为 0，实际 %d", accountTokenLimit(pro))
	}
	if windowOverLimit(pro, testModel) {
		t.Fatal("标准版不该被判越线")
	}

	for i := 0; i < 30; i++ {
		acc, _, err := nextAccountForModel(testModel, nil, "")
		if err != nil {
			t.Fatalf("应路由到标准版，实际报错：%v", err)
		}
		if acc.Path != "pro.json" {
			t.Fatalf("体验版全越线时应集中到标准版，实际 %s（第 %d 次）", acc.Path, i+1)
		}
	}
}
