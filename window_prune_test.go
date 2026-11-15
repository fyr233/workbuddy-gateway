package main

import (
	"testing"
	"time"
)

// 24 小时窗口的边界必须按「小时桶」算，不能按秒差算。
// 修复前 cut := hour - tokenWindowHours 只减了 24 秒，
// 除当前小时外的所有桶都被裁掉，软限形同虚设。
func TestPruneKeepsFull24HourWindow(t *testing.T) {
	hour := int64(1794736800) // 某个整点
	w := &accountTokenWindow{Path: "x", Buckets: []tokenBucket{
		{Hour: hour - 3600*25, Tokens: 1_000_000}, // 窗口外，应删
		{Hour: hour - 3600*23, Tokens: 2_000_000}, // 窗口内
		{Hour: hour - 3600*12, Tokens: 3_000_000}, // 窗口内
		{Hour: hour - 3600, Tokens: 4_000_000},    // 窗口内
		{Hour: hour, Tokens: 5_000_000},           // 当前小时
	}}
	pruneBucketsLocked(w, hour)
	if len(w.Buckets) != 4 {
		t.Fatalf("应保留 4 个桶（23h/12h/1h/当前），实际 %d", len(w.Buckets))
	}
	if got := windowTotalLocked(w); got != 14_000_000 {
		t.Fatalf("窗口合计应为 14M，实际 %d", got)
	}
}

// 边界：正好 24 小时前的那一小时应被丢弃（cut 是开区间）。
func TestPruneDropsExactly24HoursAgo(t *testing.T) {
	hour := int64(1794736800)
	w := &accountTokenWindow{Path: "x", Buckets: []tokenBucket{
		{Hour: hour - 3600*24, Tokens: 9_000_000},
		{Hour: hour - 3600*23, Tokens: 1_000_000},
	}}
	pruneBucketsLocked(w, hour)
	if windowTotalLocked(w) != 1_000_000 {
		t.Fatalf("24 小时前的桶应被丢弃，实际合计 %d", windowTotalLocked(w))
	}
}

// 软限要能真的触发：窗口内累计超过阈值时 windowOverLimit 必须为真。
func TestWindowOverLimitTriggersOn24hAccumulation(t *testing.T) {
	resetWindowForTest(t, "cn.json")
	defer resetWindowForTest(t, "cn.json")

	hour := time.Now().Truncate(time.Hour).Unix()
	acc := &Account{Path: "cn.json", Auth: &StoredAuth{Edition: "cn"}}
	limit := accountTokenLimit(acc)
	if limit <= 0 {
		t.Fatalf("免费 CN 账号应有软阈值，实际 %d", limit)
	}
	// 分 20 个小时桶累加，刚好超过阈值。
	per := limit/20 + 1
	for i := 0; i < 20; i++ {
		recordTokensAt(acc, hour-int64(i)*3600, per)
	}
	total, gotLimit := accountWindowUsage(acc)
	if total < limit {
		t.Fatalf("累计 %d 应已达阈值 %d", total, limit)
	}
	if !windowOverLimit(acc) {
		t.Fatalf("累计 %d >= 阈值 %d，应判定越线", total, gotLimit)
	}
}

// 落在窗口外的旧桶不能计入，否则越线判断会虚高。
func TestWindowIgnoresExpiredBuckets(t *testing.T) {
	resetWindowForTest(t, "intl.json")
	defer resetWindowForTest(t, "intl.json")

	hour := time.Now().Truncate(time.Hour).Unix()
	acc := &Account{Path: "intl.json", Auth: &StoredAuth{Edition: "intl"}}
	recordTokensAt(acc, hour-3600*26, accountTokenLimit(acc)*2)
	total, _ := accountWindowUsage(acc)
	if total != 0 {
		t.Fatalf("26 小时前的桶已过期，不应计入，实际 %d", total)
	}
	if windowOverLimit(acc) {
		t.Fatal("只有过期数据时不应判定越线")
	}
}

// 阈值必须与实测一致：CN 200M 的 95%、INTL 100M 的 95%。
func TestWindowLimitsMatchMeasured(t *testing.T) {
	cn := &Account{Path: "a.json", Auth: &StoredAuth{Edition: "cn"}}
	intl := &Account{Path: "b.json", Auth: &StoredAuth{Edition: "intl"}}
	if got := accountTokenLimit(cn); got != 190_000_000 {
		t.Fatalf("CN 软阈值应为 190M，实际 %d", got)
	}
	if got := accountTokenLimit(intl); got != 95_000_000 {
		t.Fatalf("INTL 软阈值应为 95M，实际 %d", got)
	}
	if tokenWindowHours != 24 {
		t.Fatalf("窗口应为 24 小时，实际 %d", tokenWindowHours)
	}
}

// 付费账号限流规则未知：不猜值，直接不纳入软限。
func TestPaidAccountNotWindowsLimited(t *testing.T) {
	paid := []*Account{
		{Path: "p1.json", IsPaidUser: true, Auth: &StoredAuth{Edition: "cn"}},
		{Path: "p2.json", PlanLabel: "专业版", Auth: &StoredAuth{Edition: "cn"}},
		{Path: "p3.json", IsPaidUser: true, Auth: &StoredAuth{Edition: "intl"}},
	}
	for _, acc := range paid {
		if got := accountTokenLimit(acc); got != 0 {
			t.Fatalf("%s 是付费账号，限流规则未知，应返回 0（不纳入软限），实际 %d", acc.Path, got)
		}
		// 不纳入软限意味着永远不越线，即使窗口里堆了大量 token。
		resetWindowForTest(t, acc.Path)
		recordTokensAt(acc, time.Now().Truncate(time.Hour).Unix(), 1_000_000_000)
		if windowOverLimit(acc) {
			t.Fatalf("%s 不应因窗口用量被判定越线", acc.Path)
		}
		resetWindowForTest(t, acc.Path)
	}
	// 体验版/免费账号仍然受限，不能把这条兜底扩大化。
	free := &Account{Path: "f.json", PlanLabel: "体验版", Auth: &StoredAuth{Edition: "cn"}}
	if accountTokenLimit(free) != 190_000_000 {
		t.Fatalf("体验版应按免费账号受限，实际 %d", accountTokenLimit(free))
	}
}

func resetWindowForTest(t *testing.T, path string) {
	t.Helper()
	tokenWindowMu.Lock()
	delete(tokenWindows, path)
	tokenWindowMu.Unlock()
}

// recordTokensAt 按指定小时戳写入计数（绕过 recordAccountTokens 的 time.Now）。
func recordTokensAt(acc *Account, hour int64, tokens int64) {
	tokenWindowMu.Lock()
	defer tokenWindowMu.Unlock()
	w := tokenWindows[acc.Path]
	if w == nil {
		w = &accountTokenWindow{Path: acc.Path, Edition: accountEditionOf(acc)}
		tokenWindows[acc.Path] = w
	}
	pruneBucketsLocked(w, hour)
	for i := range w.Buckets {
		if w.Buckets[i].Hour == hour {
			w.Buckets[i].Tokens += tokens
			return
		}
	}
	w.Buckets = append(w.Buckets, tokenBucket{Hour: hour, Tokens: tokens})
}
