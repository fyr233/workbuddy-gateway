package main

import (
	"testing"
	"time"
)

// resetScheduler 清空调度器的全局状态，避免测试之间互相污染。
func resetScheduler(t *testing.T, accs ...*Account) {
	t.Helper()
	accountMu.Lock()
	accounts = accs
	rrIndex = 0
	accountMu.Unlock()
	weightMu.Lock()
	weightCounters = map[string]int64{}
	weightMu.Unlock()
}

func creditAccount(path string, remain float64, hours float64, now time.Time) *Account {
	return &Account{
		Path:           path,
		Auth:           &StoredAuth{},
		CreditRemain:   remain,
		CreditGrants:   1,
		CreditExpireAt: now.Add(time.Duration(hours * float64(time.Hour))).Unix(),
	}
}

// 按权重实际派发请求，统计每个账号拿到的份额，应与 weight/horizon 成正比。
func TestSchedulingDistributionFollowsWeights(t *testing.T) {
	now := time.Now()
	accs := []*Account{
		creditAccount("a.json", 2336, 300, now),
		creditAccount("b.json", 2195, 360, now),
		creditAccount("c.json", 2733, 210, now),
	}
	resetScheduler(t, accs...)
	defer resetScheduler(t)

	const draws = 30000
	got := map[string]int{}
	for i := 0; i < draws; i++ {
		acc, _, err := nextAccountForModel("m", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		got[acc.Path]++
	}

	var totalW float64
	ws := make([]float64, len(accs))
	for i, a := range accs {
		ws[i] = accountPriorityWeight(a, time.Now())
		totalW += ws[i]
	}
	for i, a := range accs {
		wantShare := ws[i] / totalW
		gotShare := float64(got[a.Path]) / draws
		ratio := gotShare / wantShare
		t.Logf("%s 期望 %.1f%% 实际 %.1f%%（比值 %.3f）", a.Path, wantShare*100, gotShare*100, ratio)
		// 30000 次抽样下 5% 容差足够严格，同时不因加权轮询的离散性误报。
		if ratio < 0.95 || ratio > 1.05 {
			t.Errorf("%s 实际份额 %0.3f 偏离期望 %0.3f", a.Path, gotShare, wantShare)
		}
	}
}

// 旧口径会把总额大、余额少的账号排前面。新口径必须让它让位。
func TestSchedulingAvoidsExhaustedAccount(t *testing.T) {
	now := time.Now()
	spent := &Account{
		Path: "spent.json", Auth: &StoredAuth{},
		QuotaTotal: 7600, QuotaRemaining: 0, QuotaKnown: true, QuotaExhausted: true,
		ModelStates: map[string]*modelRuntimeState{"m": {CostClass: modelCostPaid}},
		CreditRemain: 0, CreditGrants: 0,
	}
	fresh := creditAccount("fresh.json", 500, 200, now)
	resetScheduler(t, spent, fresh)
	defer resetScheduler(t)

	for i := 0; i < 200; i++ {
		acc, _, err := nextAccountForModel("m", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if acc.Path != "fresh.json" {
			t.Fatalf("第 %d 次选中了已耗尽账号 %s", i, acc.Path)
		}
	}
}

// 有余额的账号：快过期的必须比晚过期的拿到更多请求。
func TestSchedulingFavoursNearerDeadline(t *testing.T) {
	now := time.Now()
	soon := creditAccount("soon.json", 1000, 10, now)
	late := creditAccount("late.json", 1000, 200, now)
	resetScheduler(t, soon, late)
	defer resetScheduler(t)

	counts := map[string]int{}
	for i := 0; i < 5000; i++ {
		acc, _, err := nextAccountForModel("m", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		counts[acc.Path]++
	}
	if counts["soon.json"] <= counts["late.json"]*10 {
		t.Fatalf("快过期账号应显著多吃：soon=%d late=%d", counts["soon.json"], counts["late.json"])
	}
	t.Logf("soon=%d late=%d", counts["soon.json"], counts["late.json"])
}

// 端到端目标验证：在「总需求小于总供给」的正常场景下，
// 各账号的消耗比例应与 1/期限 成正比 —— 期限越近消耗得越狠，
// 于是谁都不会被提前打空，也不会剩下积分过期作废。
func TestBurnDownProportionalToDeadline(t *testing.T) {
	now := time.Now()
	spec := []struct {
		path   string
		remain float64
		hours  float64
	}{
		{"a.json", 2400, 96},
		{"b.json", 1800, 240},
		{"c.json", 600, 24},
	}
	accs := make([]*Account, len(spec))
	for i, s := range spec {
		accs[i] = creditAccount(s.path, s.remain, s.hours, now)
	}
	resetScheduler(t, accs...)
	defer resetScheduler(t)

	remaining := map[string]float64{}
	var capacity float64
	for _, s := range spec {
		remaining[s.path] = s.remain
		capacity += s.remain
	}

	// 只消耗总供给的 3/4：如果分配失衡，会有账号先被打空、而别的账号还剩一大半。
	const burnPerStep = 0.5
	budget := capacity * 0.75
	for used := 0.0; used < budget; used += burnPerStep {
		acc, _, err := nextAccountForModel("m", nil, "")
		if err != nil {
			t.Fatalf("消耗 %.0f 时选号失败: %v", used, err)
		}
		accountMu.Lock()
		remaining[acc.Path] -= burnPerStep
		if remaining[acc.Path] < 0 {
			remaining[acc.Path] = 0
		}
		acc.CreditRemain = remaining[acc.Path]
		accountMu.Unlock()
	}

	for _, s := range spec {
		f := remaining[s.path] / s.remain
		t.Logf("%-8s 剩余率 %.1f%%（期限 %.0fh，余额 %.0f → %.1f）",
			s.path, f*100, s.hours, s.remain, remaining[s.path])
		// 「没有账号率先耗尽」在这里表现为：谁都没归零。
		// weight=余额/期限 是比例反馈，余额下降则权重下降，因此不存在有限时间内被打空。
		if remaining[s.path] <= 0 {
			t.Errorf("%s 已被打空，目标要求没有账号率先耗尽", s.path)
		}
	}
	// 顺序必须由「期限」决定，而不是由余额大小决定：
	// 期限最近的 c（24h）剩余率最低，期限最远的 b（240h）剩余率最高。
	fc := remaining["c.json"] / 600
	fa := remaining["a.json"] / 2400
	fb := remaining["b.json"] / 1800
	if !(fc < fa && fa < fb) {
		t.Fatalf("剩余率未按期限排序：c=%.3f a=%.3f b=%.3f", fc, fa, fb)
	}
	// b 的余额（1800）比 a（2400）少，但因期限远，剩余率必须更高 ——
	// 说明排序听的是到期时间而不是余额大小，这正是旧口径的问题所在。
	if fb <= fa {
		t.Fatalf("余额少的 b 应因期限远而保留更多：b=%.3f a=%.3f", fb, fa)
	}
}

// 越过软线的账号应被压到最低权重，避免撞上游硬墙。
// 惩罚按模型判定：被压低的只有越线的那个模型，别的模型不受牵连。
func TestPriorityWeightPenalisesWindowOverLimit(t *testing.T) {
	now := time.Now()
	acc := creditAccount("hot.json", 1000, 50, now)

	base := accountPriorityWeight(acc, now)

	// 造一个已越过软线的窗口：直接塞入超过阈值的分桶。
	tokenWindowMu.Lock()
	tokenWindows[acc.Path] = &accountTokenWindow{
		Path:    acc.Path,
		Edition: "cn",
		ModelBuckets: map[string][]tokenBucket{
			testModel: {{Hour: now.Truncate(time.Hour).Unix(), Tokens: 200_000_000}},
		},
	}
	tokenWindowMu.Unlock()
	defer func() {
		tokenWindowMu.Lock()
		delete(tokenWindows, acc.Path)
		tokenWindowMu.Unlock()
	}()

	penalised := accountPriorityWeightForModel(acc, testModel, now)
	if penalised > base*0.02 || penalised < base*0.005 {
		t.Fatalf("软线惩罚倍率异常：base=%v penalised=%v", base, penalised)
	}
	t.Logf("base=%v penalised=%v 倍率=%.4f", base, penalised, penalised/base)

	// 同一账号在没越线的模型上必须拿到全权重。
	other := accountPriorityWeightForModel(acc, "glm-x", now)
	if other != base {
		t.Fatalf("未越线的模型不该被连带降权：base=%v other=%v", base, other)
	}
}
