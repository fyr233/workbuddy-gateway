package main

import (
	"testing"
	"time"
)

// freeSiteFixture 构造「国际站免费、国内站收费」的模型目录，供选号测试使用。
func freeSiteFixture(t *testing.T) {
	t.Helper()
	resetModelsStateT(t)
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn":   {{ID: "free-intl-model", Multiplier: 0.03, HasMultiplier: true, FromLive: true}},
		"intl": {{ID: "free-intl-model", Multiplier: 0, HasMultiplier: true, PromoFree: true, FromLive: true}},
	}
	modelProbes = map[string]modelPriceProbe{}
	modelsMu.Unlock()
}

// 免费站点优先 + 48h 软限必须同时生效：
// 免费站点只是把候选收窄到 intl，软限仍在收窄后的集合里排除越线账号。
func TestFreeSitePreferenceRespectsWindowLimit(t *testing.T) {
	resetRoutingState(t)
	freeSiteFixture(t)
	oldDisabled := windowDisabled
	windowDisabled = false
	t.Cleanup(func() {
		windowDisabled = oldDisabled
		tokenWindowMu.Lock()
		tokenWindows = map[string]*accountTokenWindow{}
		tokenWindowMu.Unlock()
	})

	intl1 := &Account{Path: "intl1.json", Edition: "intl",
		Auth: &StoredAuth{Edition: "intl", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}}}
	intl2 := &Account{Path: "intl2.json", Edition: "intl",
		Auth: &StoredAuth{Edition: "intl", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}}}
	cn1 := &Account{Path: "cn1.json", Edition: "cn",
		Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}}}
	withAccountPool(t, []*Account{intl1, intl2, cn1})

	if pref := preferredFreeSites("free-intl-model"); pref == nil || !pref["intl"] {
		t.Fatalf("intl 应被判定为免费站点: %v", pref)
	}

	hour := time.Now().Truncate(time.Hour).Unix()
	tokenWindowMu.Lock()
	// intl1 已越过国际站软线，intl2 未越线
	tokenWindows["intl1.json"] = &accountTokenWindow{Path: "intl1.json", Edition: "intl",
		Buckets: []tokenBucket{{Hour: hour, Tokens: defaultWindowLimitIntl + 1}}}
	tokenWindowMu.Unlock()

	if !windowOverLimit(intl1) {
		t.Fatal("intl1 应判定越线")
	}

	// 选号：应落在未越线的 intl2，而不是越线的 intl1，也不该跳到收费的 cn1
	for i := 0; i < 10; i++ {
		acc, _, err := nextAccountForModel("free-intl-model", nil, "")
		if err != nil {
			t.Fatalf("选号失败: %v", err)
		}
		if acc.Path != "intl2.json" {
			t.Fatalf("应选未越线的免费站点账号 intl2.json，实际 %s", acc.Path)
		}
	}
}

// 免费站点全部越线后，应切到收费站点代偿，而不是失败。
func TestFreeSiteExhaustedFallsBackToPaidSite(t *testing.T) {
	resetRoutingState(t)
	freeSiteFixture(t)
	oldDisabled := windowDisabled
	windowDisabled = false
	t.Cleanup(func() {
		windowDisabled = oldDisabled
		tokenWindowMu.Lock()
		tokenWindows = map[string]*accountTokenWindow{}
		tokenWindowMu.Unlock()
	})

	intl1 := &Account{Path: "intl1.json", Edition: "intl",
		Auth: &StoredAuth{Edition: "intl", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}}}
	cn1 := &Account{Path: "cn1.json", Edition: "cn",
		Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}}}
	withAccountPool(t, []*Account{intl1, cn1})

	hour := time.Now().Truncate(time.Hour).Unix()
	tokenWindowMu.Lock()
	tokenWindows["intl1.json"] = &accountTokenWindow{Path: "intl1.json", Edition: "intl",
		Buckets: []tokenBucket{{Hour: hour, Tokens: defaultWindowLimitIntl + 1}}}
	tokenWindowMu.Unlock()

	acc, _, err := nextAccountForModel("free-intl-model", nil, "")
	if err != nil {
		t.Fatalf("免费站点越线后应切到收费站点，实际报错: %v", err)
	}
	if acc.Path != "cn1.json" {
		t.Fatalf("应切到 cn1.json 代偿，实际 %s", acc.Path)
	}
}

// 软限阈值按站点+付费状态分档，四档都必须正确。
func TestAccountTokenLimitTiers(t *testing.T) {
	oldDisabled := windowDisabled
	windowDisabled = false
	t.Cleanup(func() { windowDisabled = oldDisabled })

	cases := []struct {
		name string
		acc  *Account
		want int64
	}{
		{"国际站免费", &Account{Edition: "intl"}, defaultWindowLimitIntl},
		{"国内站免费", &Account{Edition: "cn"}, defaultWindowLimitCN},
		{"国际站付费", &Account{Edition: "intl", IsPaidUser: true}, defaultWindowLimitIntlPaid},
		{"国内站付费", &Account{Edition: "cn", IsPaidUser: true}, defaultWindowLimitCNPaid},
		{"Auth 里的 edition 优先", &Account{Edition: "cn", Auth: &StoredAuth{Edition: "intl"}}, defaultWindowLimitIntl},
		{"global 等同国际站", &Account{Edition: "global"}, defaultWindowLimitIntl},
	}
	for _, c := range cases {
		if got := accountTokenLimit(c.acc); got != c.want {
			t.Errorf("%s: 阈值=%d 期望=%d", c.name, got, c.want)
		}
	}
}

// 关闭开关时阈值返回 0，表示软限整体失效。
func TestWindowDisabledYieldsNoLimit(t *testing.T) {
	oldDisabled := windowDisabled
	windowDisabled = true
	t.Cleanup(func() { windowDisabled = oldDisabled })

	acc := &Account{Edition: "cn"}
	if got := accountTokenLimit(acc); got != 0 {
		t.Fatalf("关闭后应返回 0，实际 %d", got)
	}
	if windowOverLimit(acc) {
		t.Fatal("关闭后不应判定越线")
	}
	if got := windowHeadroom(acc); got != 1<<62 {
		t.Fatalf("关闭后余量应为无穷，实际 %d", got)
	}
}

// 软限是软排除：越过软线的账号在还有别的可用账号时不被选中，
// 但一旦只剩它一个，仍必须能服务（不能整段不可用）。
func TestWindowLimitIsSoftNotHard(t *testing.T) {
	resetRoutingState(t)
	oldDisabled := windowDisabled
	windowDisabled = false
	t.Cleanup(func() {
		windowDisabled = oldDisabled
		tokenWindowMu.Lock()
		tokenWindows = map[string]*accountTokenWindow{}
		tokenWindowMu.Unlock()
	})

	only := testAccount("only.json", false, 0)
	withAccountPool(t, []*Account{only})

	hour := time.Now().Truncate(time.Hour).Unix()
	tokenWindowMu.Lock()
	tokenWindows["only.json"] = &accountTokenWindow{Path: "only.json", Edition: "cn",
		Buckets: []tokenBucket{{Hour: hour, Tokens: defaultWindowLimitCN + 5_000_000}}}
	tokenWindowMu.Unlock()

	acc, _, err := nextAccountForModel("m", nil, "")
	if err != nil {
		t.Fatalf("唯一账号越软线时必须仍能服务，实际报错: %v", err)
	}
	if acc.Path != "only.json" {
		t.Fatalf("应回落到 only.json，实际 %s", acc.Path)
	}
}
