package main

import (
	"testing"
	"time"
)

const testModel = "deepseek-v4.1-flash"

// 24 小时窗口的边界必须按「小时桶」算，不能按秒差算。
// 修复前 cut := hour - tokenWindowHours 只减了 24 秒，
// 除当前小时外的所有桶都被裁掉，软限形同虚设。
func TestPruneKeepsFull24HourWindow(t *testing.T) {
	hour := int64(1794736800) // 某个整点
	w := &accountTokenWindow{Path: "x", ModelBuckets: map[string][]tokenBucket{
		testModel: {
			{Hour: hour - 3600*25, Tokens: 1_000_000}, // 窗口外，应删
			{Hour: hour - 3600*23, Tokens: 2_000_000}, // 窗口内
			{Hour: hour - 3600*12, Tokens: 3_000_000}, // 窗口内
			{Hour: hour - 3600, Tokens: 4_000_000},    // 窗口内
			{Hour: hour, Tokens: 5_000_000},           // 当前小时
		},
	}}
	pruneBucketsLocked(w, hour)
	if len(w.ModelBuckets[testModel]) != 4 {
		t.Fatalf("应保留 4 个桶（23h/12h/1h/当前），实际 %d", len(w.ModelBuckets[testModel]))
	}
	if got := windowTotalLocked(w, testModel); got != 14_000_000 {
		t.Fatalf("窗口合计应为 14M，实际 %d", got)
	}
}

// 边界：正好 24 小时前的那一小时应被丢弃（cut 是开区间）。
func TestPruneDropsExactly24HoursAgo(t *testing.T) {
	hour := int64(1794736800)
	w := &accountTokenWindow{Path: "x", ModelBuckets: map[string][]tokenBucket{
		testModel: {
			{Hour: hour - 3600*24, Tokens: 9_000_000},
			{Hour: hour - 3600*23, Tokens: 1_000_000},
		},
	}}
	pruneBucketsLocked(w, hour)
	if got := windowTotalLocked(w, testModel); got != 1_000_000 {
		t.Fatalf("24 小时前的桶应被丢弃，实际合计 %d", got)
	}
}

// 旧落盘文件只有账号级 Buckets、没有模型维度，无法拆分：必须丢弃而不是叠加，
// 否则会与新口径双计。
func TestPruneDropsLegacyAccountLevelBuckets(t *testing.T) {
	hour := int64(1794736800)
	w := &accountTokenWindow{Path: "x", Buckets: []tokenBucket{
		{Hour: hour, Tokens: 50_000_000},
	}}
	pruneBucketsLocked(w, hour)
	if len(w.Buckets) != 0 {
		t.Fatalf("旧格式账号级桶应被清空，实际还剩 %d 个", len(w.Buckets))
	}
	if len(w.ModelBuckets) != 0 {
		t.Fatalf("不应把旧桶挪进模型维度（会双计），实际 %d 个模型", len(w.ModelBuckets))
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
		recordTokensAt(acc, testModel, hour-int64(i)*3600, per)
	}
	total, gotLimit := accountWindowUsage(acc, testModel)
	if total < limit {
		t.Fatalf("累计 %d 应已达阈值 %d", total, limit)
	}
	if !windowOverLimit(acc, testModel) {
		t.Fatalf("累计 %d >= 阈值 %d，应判定越线", total, gotLimit)
	}
}

// 核心回归：软限必须按模型分开计。
//
// 上游 6004 是按模型限流的（实测 10 次触发点，同账号「其它模型」用量均为 0.0M）。
// 改成按模型分桶后，跑小模型不能吃掉大模型的软限预算，否则大模型会提前被软排除、
// 反而更快撞上游硬墙 —— 这正是修复前的行为。
func TestWindowIsPerModelNotPerAccount(t *testing.T) {
	const path = "permodel.json"
	resetWindowForTest(t, path)
	defer resetWindowForTest(t, path)

	hour := time.Now().Truncate(time.Hour).Unix()
	acc := &Account{Path: path, Auth: &StoredAuth{Edition: "cn"}}
	limit := accountTokenLimit(acc)

	// 另一个模型把预算几乎用光。
	recordTokensAt(acc, "glm-x", hour, limit*2)
	if !windowOverLimit(acc, "glm-x") {
		t.Fatalf("glm-x 累计 %d 应越线", limit*2)
	}
	// deepseek 一个 token 都没用，必须完全不受影响。
	if got, _ := accountWindowUsage(acc, testModel); got != 0 {
		t.Fatalf("深求模型用量应为 0（其它模型的量不能串进来），实际 %d", got)
	}
	if windowOverLimit(acc, testModel) {
		t.Fatal("glm-x 越线不应让 deepseek 也被判定越线")
	}

	// 反向确认：deepseek 自己用满后必须越线。
	recordTokensAt(acc, testModel, hour, limit)
	if !windowOverLimit(acc, testModel) {
		t.Fatalf("deepseek 累计 %d >= 阈值，应越线", limit)
	}
}

// 落在窗口外的旧桶不能计入，否则越线判断会虚高。
func TestWindowIgnoresExpiredBuckets(t *testing.T) {
	resetWindowForTest(t, "intl.json")
	defer resetWindowForTest(t, "intl.json")

	hour := time.Now().Truncate(time.Hour).Unix()
	acc := &Account{Path: "intl.json", Auth: &StoredAuth{Edition: "intl"}}
	recordTokensAt(acc, testModel, hour-3600*26, accountTokenLimit(acc)*2)
	total, _ := accountWindowUsage(acc, testModel)
	if total != 0 {
		t.Fatalf("26 小时前的桶已过期，不应计入，实际 %d", total)
	}
	if windowOverLimit(acc, testModel) {
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
		recordTokensAt(acc, testModel, time.Now().Truncate(time.Hour).Unix(), 1_000_000_000)
		if windowOverLimit(acc, testModel) {
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

// recordTokensAt 按指定小时戳写入指定模型的计数（绕过 recordAccountTokens 的 time.Now）。
func recordTokensAt(acc *Account, model string, hour int64, tokens int64) {
	tokenWindowMu.Lock()
	defer tokenWindowMu.Unlock()
	w := tokenWindows[acc.Path]
	if w == nil {
		w = &accountTokenWindow{Path: acc.Path, Edition: accountEditionOf(acc)}
		tokenWindows[acc.Path] = w
	}
	if w.ModelBuckets == nil {
		w.ModelBuckets = make(map[string][]tokenBucket)
	}
	pruneBucketsLocked(w, hour)
	buckets := w.ModelBuckets[model]
	for i := range buckets {
		if buckets[i].Hour == hour {
			buckets[i].Tokens += tokens
			w.ModelBuckets[model] = buckets
			return
		}
	}
	w.ModelBuckets[model] = append(buckets, tokenBucket{Hour: hour, Tokens: tokens, Model: model})
}
