package main

import (
	"encoding/json"
	"testing"
	"time"
)

func res(status int, remain string, endMS int64, cycle string) planResource {
	r := planResource{
		Status:         json.RawMessage(numText(int64(status))),
		CapacityRemain: json.RawMessage(`"` + remain + `"`),
		CapacitySize:   json.RawMessage(`"100"`),
	}
	if endMS > 0 {
		r.DeductionEndTime = json.RawMessage(numText(endMS))
	}
	if cycle != "" {
		r.CycleEndTime = json.RawMessage(`"` + cycle + `"`)
	}
	return r
}

func numText(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// 已用尽(status=3)、已过期、余量为 0 的包都不能算进可用积分。
func TestCreditGrantOfFilters(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	future := now.Add(24 * time.Hour).UnixMilli()
	past := now.Add(-time.Hour).UnixMilli()

	g, ok := creditGrantOf(res(0, "54", future, ""), now)
	if !ok || g.Remain != 54 || !g.ExpireAt.Equal(time.UnixMilli(future)) {
		t.Fatalf("有效的包应被采纳: ok=%v g=%+v", ok, g)
	}
	for name, r := range map[string]planResource{
		"已用尽":  res(3, "100", future, ""),
		"已过期":  res(0, "100", past, ""),
		"余量为零": res(0, "0", future, ""),
		"已退款":  res(1, "100", future, ""),
	} {
		if _, ok := creditGrantOf(r, now); ok {
			t.Errorf("%s 的包不应参与调度", name)
		}
	}
}

// 截止时间取最早的一个：月付包 DeductionEndTime 在 2050 年，但 CycleEndTime 是本周期末，
// 后者才是剩余积分真正的作废点。若只取第一个非空字段，会把本周期末的浪费算成几十年后。
func TestCreditGrantUsesEarliestDeadline(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	r := planResource{
		Status:           json.RawMessage(`0`),
		CapacityRemain:   json.RawMessage(`"500"`),
		DeductionEndTime: json.RawMessage(`2052309803000`), // 2050 年
		CycleEndTime:     json.RawMessage(`"2026-11-30 23:59:59"`),
	}
	g, ok := creditGrantOf(r, now)
	if !ok {
		t.Fatal("应当被采纳")
	}
	want := time.Date(2026, 11, 30, 23, 59, 59, 0, time.FixedZone("UTC+8", 8*3600))
	if !g.ExpireAt.Equal(want) {
		t.Fatalf("应取本周期末 %s，实际 %s", want, g.ExpireAt)
	}
}

func TestSummarizeCredits(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	near := now.Add(2 * time.Hour).UnixMilli()
	far := now.Add(240 * time.Hour).UnixMilli()
	rows := []planResource{
		res(0, "33", near, ""),
		res(0, "100", far, ""),
		res(3, "100", near, ""), // 用尽，不计
	}
	remain, expire, n := summarizeCredits(rows, now)
	if remain != 133 || n != 2 {
		t.Fatalf("汇总错误 remain=%v n=%d", remain, n)
	}
	if !expire.Equal(time.UnixMilli(near)) {
		t.Fatalf("应取最近到期时刻 %s，实际 %s", time.UnixMilli(near), expire)
	}
}

// 核心性质：权重 = 余额 / 距到期小时数。
// 余额相同，到期越近权重越高（先烧快过期的）。
func TestPriorityWeightPrefersSoonExpiring(t *testing.T) {
	now := time.Now()
	soon := &Account{CreditRemain: 1000, CreditGrants: 1, CreditExpireAt: now.Add(10 * time.Hour).Unix()}
	late := &Account{CreditRemain: 1000, CreditGrants: 1, CreditExpireAt: now.Add(200 * time.Hour).Unix()}

	ws, wl := accountPriorityWeight(soon, now), accountPriorityWeight(late, now)
	if ws <= wl*10 {
		t.Fatalf("快过期的号权重应显著更高：soon=%v late=%v", ws, wl)
	}
}

func TestPriorityWeightScalesWithRemaining(t *testing.T) {
	now := time.Now()
	horizon := now.Add(50 * time.Hour).Unix()
	rich := &Account{CreditRemain: 5000, CreditGrants: 1, CreditExpireAt: horizon}
	poor := &Account{CreditRemain: 500, CreditGrants: 1, CreditExpireAt: horizon}

	if wr, wp := accountPriorityWeight(rich, now), accountPriorityWeight(poor, now); wr <= wp {
		t.Fatalf("余额多的号权重应更高：rich=%v poor=%v", wr, wp)
	}
}

func TestPriorityWeightZeroRemain(t *testing.T) {
	now := time.Now()
	acc := &Account{CreditRemain: 0, CreditGrants: 3, CreditExpireAt: now.Add(time.Hour).Unix()}
	if w := accountPriorityWeight(acc, now); w != priorityWeightFloor {
		t.Fatalf("无余额应压到最低权重，实际 %v", w)
	}
}

// 没有积分包明细（企业号/旧快照）时退回周期剩余，不能把账号权重打成 0。
func TestPriorityWeightFallsBackToQuotaRemaining(t *testing.T) {
	now := time.Now()
	acc := &Account{QuotaRemaining: 800, CreditGrants: 0}
	w := accountPriorityWeight(acc, now)
	if w <= priorityWeightFloor {
		t.Fatalf("无积分包明细时应按 QuotaRemaining 计算，实际 %v", w)
	}
	// 无明细时按 30 天视界折算。
	if want := 800 / priorityDefaultHorizon.Hours(); w != want {
		t.Fatalf("期望 %v，实际 %v", want, w)
	}
}

// 目标验证：按权重分配流量后，各账号在自己的到期时刻同时归零 —— 既无浪费也无提前耗尽。
func TestWeightsEqualizeBurnDown(t *testing.T) {
	now := time.Now()
	type acct struct {
		remain float64
		hours  float64
	}
	cases := []acct{
		{1000, 10},
		{5000, 100},
		{200, 3},
		{8000, 400},
	}
	weights := make([]float64, len(cases))
	var sum float64
	for i, c := range cases {
		a := &Account{CreditRemain: c.remain, CreditGrants: 1, CreditExpireAt: now.Add(time.Duration(c.hours) * time.Hour).Unix()}
		weights[i] = accountPriorityWeight(a, now)
		sum += weights[i]
	}
	// 总运力取「所有账号都恰好烧完」所需的速率。
	var totalRate float64
	for _, c := range cases {
		totalRate += c.remain / c.hours
	}
	for i, c := range cases {
		share := weights[i] / sum
		gotRate := share * totalRate
		wantRate := c.remain / c.hours
		// 允许 5% 误差（到期取整到秒引入的偏差）。
		if diff := (gotRate - wantRate) / wantRate; diff > 0.05 || diff < -0.05 {
			t.Errorf("账号 %d 分配速率 %v 与消耗需求 %v 偏差过大", i, gotRate, wantRate)
		}
	}
}
