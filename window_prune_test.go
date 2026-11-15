package main

import (
	"testing"
	"time"
)

// 48 小时窗口的边界必须按「小时桶」算，不能按秒差算。
// 修复前 cut := hour - tokenWindowHours 只减了 48 秒，
// 除当前小时外的所有桶都被裁掉，软限形同虚设。
func TestPruneKeepsFull48HourWindow(t *testing.T) {
	hour := int64(1794736800) // 某个整点
	w := &accountTokenWindow{Path: "x", Buckets: []tokenBucket{
		{Hour: hour - 3600*49, Tokens: 1_000_000}, // 窗口外，应删
		{Hour: hour - 3600*47, Tokens: 2_000_000}, // 窗口内
		{Hour: hour - 3600*24, Tokens: 3_000_000}, // 窗口内
		{Hour: hour - 3600, Tokens: 4_000_000},    // 窗口内
		{Hour: hour, Tokens: 5_000_000},           // 当前小时
	}}
	pruneBucketsLocked(w, hour)
	if len(w.Buckets) != 4 {
		t.Fatalf("应保留 4 个桶（47h/24h/1h/当前），实际 %d", len(w.Buckets))
	}
	if got := windowTotalLocked(w); got != 14_000_000 {
		t.Fatalf("窗口合计应为 14M，实际 %d", got)
	}
}

// 边界：正好 48 小时前的那一小时应被丢弃（cut 是开区间）。
func TestPruneDropsExactly48HoursAgo(t *testing.T) {
	hour := int64(1794736800)
	w := &accountTokenWindow{Path: "x", Buckets: []tokenBucket{
		{Hour: hour - 3600*48, Tokens: 9_000_000},
		{Hour: hour - 3600*47, Tokens: 1_000_000},
	}}
	pruneBucketsLocked(w, hour)
	if windowTotalLocked(w) != 1_000_000 {
		t.Fatalf("48 小时前的桶应被丢弃，实际合计 %d", windowTotalLocked(w))
	}
}

// 软限要能真的触发：47 小时内累计超过阈值时 windowOverLimit 必须为真。
func TestWindowOverLimitTriggersOn48hAccumulation(t *testing.T) {
	now := time.Now()
	hour := now.Truncate(time.Hour).Unix()
	acc := &Account{Path: "cn.json", Auth: &StoredAuth{Edition: "cn"}}
	resetWindowForTest(t, acc.Path)
	defer resetWindowForTest(t, acc.Path)

	limit := accountTokenLimit(acc)
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
	now := time.Now()
	hour := now.Truncate(time.Hour).Unix()
	acc := &Account{Path: "intl.json", Auth: &StoredAuth{Edition: "intl"}}
	resetWindowForTest(t, acc.Path)
	defer resetWindowForTest(t, acc.Path)

	recordTokensAt(acc, hour-3600*50, accountTokenLimit(acc)*2)
	total, _ := accountWindowUsage(acc)
	if total != 0 {
		t.Fatalf("50 小时前的桶已过期，不应计入，实际 %d", total)
	}
	if windowOverLimit(acc) {
		t.Fatal("只有过期数据时不应判定越线")
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
