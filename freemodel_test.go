package main

import (
	"testing"
	"time"
)

// 免费模型（credits 0.00x，如 hy3 / hy4free）不消耗积分。
// 额度已耗尽但该模型免费的账号应优先承接，此时积分权重不应把别的账号拉进来。
func TestFreeModelUsesExhaustedFreeAccount(t *testing.T) {
	now := time.Now()
	exhaustedFree := &Account{
		Path: "cn.json", Auth: &StoredAuth{},
		QuotaKnown: true, QuotaExhausted: true, QuotaRemaining: 0,
		CreditRemain: 0, CreditGrants: 0,
		ModelStates: map[string]*modelRuntimeState{"hy3": {CostClass: modelCostFree}},
	}
	rich := creditAccount("rich.json", 2500, 300, now)
	resetScheduler(t, exhaustedFree, rich)
	defer resetScheduler(t)

	for i := 0; i < 50; i++ {
		acc, kind, err := nextAccountForModel("hy3", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if acc.Path != "cn.json" || kind != selectionFreeExhausted {
			t.Fatalf("免费模型应先用免费额度的已耗尽账号，实际 %s kind=%s", acc.Path, kind)
		}
	}
}

// 免费模型下，已耗尽账号越过 48h 软线后应由有余额账号代偿，而不是继续硬撑。
func TestFreeModelFallsBackAfterWindowOverLimit(t *testing.T) {
	now := time.Now()
	exhaustedFree := &Account{
		Path: "cn.json", Auth: &StoredAuth{},
		QuotaKnown: true, QuotaExhausted: true,
		ModelStates: map[string]*modelRuntimeState{"hy3": {CostClass: modelCostFree}},
	}
	rich := creditAccount("rich.json", 2500, 300, now)
	resetScheduler(t, exhaustedFree, rich)
	defer resetScheduler(t)

	tokenWindowMu.Lock()
	tokenWindows["cn.json"] = &accountTokenWindow{
		Path: "cn.json", Edition: "cn",
		Buckets: []tokenBucket{{Hour: now.Truncate(time.Hour).Unix(), Tokens: 300_000_000}},
	}
	tokenWindowMu.Unlock()
	defer func() {
		tokenWindowMu.Lock()
		delete(tokenWindows, "cn.json")
		tokenWindowMu.Unlock()
	}()

	for i := 0; i < 20; i++ {
		acc, _, err := nextAccountForModel("hy3", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if acc.Path != "rich.json" {
			t.Fatalf("越软线后应由有余额账号代偿，实际 %s", acc.Path)
		}
	}
}

// 付费模型下，同一批账号仍按积分权重分配，免费模型的存在不改变这一点。
func TestPaidModelStillWeightedWhenFreeModelPresent(t *testing.T) {
	now := time.Now()
	a := creditAccount("a.json", 2000, 100, now)
	b := creditAccount("b.json", 500, 100, now)
	a.ModelStates = map[string]*modelRuntimeState{"hy3": {CostClass: modelCostFree}, "m": {CostClass: modelCostPaid}}
	b.ModelStates = map[string]*modelRuntimeState{"hy3": {CostClass: modelCostFree}, "m": {CostClass: modelCostPaid}}
	resetScheduler(t, a, b)
	defer resetScheduler(t)

	counts := map[string]int{}
	for i := 0; i < 5000; i++ {
		acc, kind, err := nextAccountForModel("m", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if kind != selectionNormal {
			t.Fatalf("付费模型应走 selectionNormal，实际 %s", kind)
		}
		counts[acc.Path]++
	}
	// 余额 4:1，份额应接近 80:20
	share := float64(counts["a.json"]) / 5000
	if share < 0.75 || share > 0.85 {
		t.Fatalf("付费模型份额 %0.2f，期望约 0.80（a=%d b=%d）", share, counts["a.json"], counts["b.json"])
	}
	t.Logf("a=%d b=%d share=%0.3f", counts["a.json"], counts["b.json"], share)
}
