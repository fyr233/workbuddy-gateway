package main

import (
	"fmt"
	"testing"
	"time"
)

// resetRoutingState 清空粘性绑定与加权计数，让每个用例从确定状态开始。
func resetRoutingState(t *testing.T) {
	t.Helper()
	stickyReset()
	t.Cleanup(stickyReset)
}

func testAccount(path string, paid bool, quotaTotal float64) *Account {
	return &Account{
		Path:       path,
		Auth:       &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: path, ExpiresAt: time.Now().Add(time.Hour).Unix()}},
		IsPaidUser: paid,
		QuotaTotal: quotaTotal,
	}
}

// withAccountPool 临时替换全局账号池，退出时恢复。
func withAccountPool(t *testing.T, accs []*Account) {
	t.Helper()
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	accounts, rrIndex = accs, 0
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	})
}

func TestSessionKeyFromExplicitIdentifiers(t *testing.T) {
	resetRoutingState(t)
	cases := []struct {
		name string
		body string
	}{
		{"顶层 conversation_id", `{"conversation_id":"conv-1","messages":[{"role":"user","content":"hi"}]}`},
		{"顶层 conversationId", `{"conversationId":"conv-1","messages":[{"role":"user","content":"hi"}]}`},
		{"顶层 session_id", `{"session_id":"conv-1","messages":[{"role":"user","content":"hi"}]}`},
		{"metadata.conversation_id", `{"metadata":{"conversation_id":"conv-1"},"messages":[{"role":"user","content":"hi"}]}`},
		{"metadata.user_id", `{"metadata":{"user_id":"conv-1"},"messages":[{"role":"user","content":"hi"}]}`},
	}
	var first string
	for i, c := range cases {
		key := sessionKeyFromBody([]byte(c.body))
		if key == "" {
			t.Fatalf("%s: 未派生出会话键", c.name)
		}
		if i == 0 {
			first = key
			continue
		}
		if key != first {
			t.Fatalf("%s: 同一会话标识派生出了不同键 %q != %q", c.name, key, first)
		}
	}
}

// 通用 OpenAI 客户端不发会话标识，但每轮重发完整历史：
// 「system + 最早一条 user」在多轮之间保持稳定，必须派生出同一个键。
func TestSessionKeyStableAcrossGrowingHistory(t *testing.T) {
	resetRoutingState(t)
	turn1 := `{"messages":[
		{"role":"system","content":"你是助手"},
		{"role":"user","content":"第一个问题"}]}`
	turn3 := `{"messages":[
		{"role":"system","content":"你是助手"},
		{"role":"user","content":"第一个问题"},
		{"role":"assistant","content":"回答一"},
		{"role":"user","content":"第二个问题"},
		{"role":"assistant","content":"回答二"},
		{"role":"user","content":"第三个问题"}]}`

	k1 := sessionKeyFromBody([]byte(turn1))
	k3 := sessionKeyFromBody([]byte(turn3))
	if k1 == "" || k3 == "" {
		t.Fatalf("派生失败: k1=%q k3=%q", k1, k3)
	}
	if k1 != k3 {
		t.Fatalf("同一会话随历史增长派生出了不同键: %q != %q", k1, k3)
	}
}

// 不同会话必须落到不同键，否则粘性会把所有流量压到一个账号。
func TestSessionKeyDiffersBetweenConversations(t *testing.T) {
	resetRoutingState(t)
	a := `{"messages":[{"role":"user","content":"问题 A"}]}`
	b := `{"messages":[{"role":"user","content":"问题 B"}]}`
	ka, kb := sessionKeyFromBody([]byte(a)), sessionKeyFromBody([]byte(b))
	if ka == "" || kb == "" {
		t.Fatalf("派生失败: %q %q", ka, kb)
	}
	if ka == kb {
		t.Fatal("不同会话派生出了相同的键")
	}
}

// 内容块数组（多模态/结构化内容）也要能派生，且与字符串形式等价。
func TestSessionKeyContentBlocksEquivalentToString(t *testing.T) {
	resetRoutingState(t)
	plain := `{"messages":[{"role":"user","content":"同样的文本"}]}`
	blocks := `{"messages":[{"role":"user","content":[{"type":"text","text":"同样的文本"}]}]}`
	if a, b := sessionKeyFromBody([]byte(plain)), sessionKeyFromBody([]byte(blocks)); a != b {
		t.Fatalf("字符串与内容块派生不一致: %q != %q", a, b)
	}
}

func TestSessionKeyEmptyForUnusableBody(t *testing.T) {
	resetRoutingState(t)
	for _, body := range []string{"", "{}", "not-json", `{"messages":[]}`} {
		if key := sessionKeyFromBody([]byte(body)); key != "" {
			t.Fatalf("请求体 %q 不该派生出会话键，得到 %q", body, key)
		}
	}
}

// 超过上限的长内容截断后必须是合法 UTF-8，否则哈希输入不稳定、日志乱码。
func TestLimitRunesKeepsUTF8Boundary(t *testing.T) {
	resetRoutingState(t)
	long := ""
	for i := 0; i < stickyContentLimit; i++ {
		long += "汉"
	}
	got := limitRunes(long, 7) // 7 不是 3 的倍数，正好落在汉字中间
	if len(got) > 7 {
		t.Fatalf("截断后超长: %d", len(got))
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatal("截断产生了替换字符，说明劈开了多字节字符")
		}
	}
	if len(got) != 6 {
		t.Fatalf("应按完整字符回退到 6 字节，实际 %d 字节: %q", len(got), got)
	}
}

// 同一会话连续选号必须固定同一账号（这是前缀缓存命中的前提）。
func TestStickyRoutesSameSessionToSameAccount(t *testing.T) {
	resetRoutingState(t)
	a := testAccount("a.json", false, 3000)
	b := testAccount("b.json", false, 3000)
	c := testAccount("c.json", false, 3000)
	withAccountPool(t, []*Account{a, b, c})

	key := "sc-x-test-session"
	var first string
	for i := 0; i < 12; i++ {
		acc, _, err := nextAccountForModel("m", nil, key)
		if err != nil {
			t.Fatalf("第 %d 次选号失败: %v", i, err)
		}
		if i == 0 {
			first = acc.Path
			continue
		}
		if acc.Path != first {
			t.Fatalf("同一会话被分到了不同账号: 第 %d 次=%s，首次=%s", i, acc.Path, first)
		}
	}
}

// 不同会话应能分散到不同账号，粘性不能退化成「全部压一个账号」。
func TestStickyDoesNotCollapseAllSessionsToOneAccount(t *testing.T) {
	resetRoutingState(t)
	a := testAccount("a.json", false, 3000)
	b := testAccount("b.json", false, 3000)
	c := testAccount("c.json", false, 3000)
	withAccountPool(t, []*Account{a, b, c})

	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		acc, _, err := nextAccountForModel("m", nil, fmt.Sprintf("sc-x-session-%d", i))
		if err != nil {
			t.Fatalf("选号失败: %v", err)
		}
		seen[acc.Path] = true
	}
	if len(seen) < 2 {
		t.Fatalf("30 个不同会话只落到 %d 个账号，加权选择可能失效: %v", len(seen), seen)
	}
}

// 绑定的账号进入冷却后必须解绑并换号，不能一直往坏账号上撞。
func TestStickyRebindsWhenBoundAccountCoolsDown(t *testing.T) {
	resetRoutingState(t)
	a := testAccount("a.json", false, 3000)
	b := testAccount("b.json", false, 3000)
	withAccountPool(t, []*Account{a, b})

	key := "sc-x-cooling"
	first, _, err := nextAccountForModel("m", nil, key)
	if err != nil {
		t.Fatalf("首次选号失败: %v", err)
	}
	// 让被绑定的账号进入冷却
	accountMu.Lock()
	first.CooldownUntil = time.Now().Add(time.Hour)
	accountMu.Unlock()

	second, _, err := nextAccountForModel("m", nil, key)
	if err != nil {
		t.Fatalf("冷却后选号失败: %v", err)
	}
	if second.Path == first.Path {
		t.Fatalf("绑定账号已冷却，仍被选中: %s", second.Path)
	}
	if bound := stickyLookup(key, time.Now()); bound != second.Path {
		t.Fatalf("解绑后未改绑到新账号: bound=%q want=%q", bound, second.Path)
	}
}

// 本轮回已试过的账号不能再被粘性命中，否则重试会死循环。
func TestStickySkipsAlreadyAttemptedAccount(t *testing.T) {
	resetRoutingState(t)
	a := testAccount("a.json", false, 3000)
	b := testAccount("b.json", false, 3000)
	withAccountPool(t, []*Account{a, b})

	key := "sc-x-attempted"
	first, _, err := nextAccountForModel("m", nil, key)
	if err != nil {
		t.Fatalf("首次选号失败: %v", err)
	}
	attempted := map[*Account]bool{}
	accountMu.Lock()
	for _, acc := range accounts {
		if acc.Path == first.Path {
			attempted[acc] = true
		}
	}
	accountMu.Unlock()

	next, _, err := nextAccountForModel("m", attempted, key)
	if err != nil {
		t.Fatalf("重试选号失败: %v", err)
	}
	if next.Path == first.Path {
		t.Fatalf("已试过的账号被再次选中: %s", next.Path)
	}
}

// 粘性 TTL 到期后绑定必须失效。
func TestStickyBindingExpires(t *testing.T) {
	resetRoutingState(t)
	oldTTL := stickyTTL
	stickyTTL = 50 * time.Millisecond
	t.Cleanup(func() { stickyTTL = oldTTL })

	key := "sc-x-ttl"
	stickyBind(key, "a.json", time.Now())
	if bound := stickyLookup(key, time.Now()); bound != "a.json" {
		t.Fatalf("绑定未生效: %q", bound)
	}
	time.Sleep(80 * time.Millisecond)
	if bound := stickyLookup(key, time.Now()); bound != "" {
		t.Fatalf("绑定已过期仍返回账号: %q", bound)
	}
}

// 加权轮询：付费号（7000）吃到的请求数应明显多于免费号（3000）。
func TestWeightedRoundRobinFavorsPaidAccount(t *testing.T) {
	resetRoutingState(t)
	paid := testAccount("paid.json", true, 0)  // 权重 7000
	free := testAccount("free.json", false, 0) // 权重 3000
	withAccountPool(t, []*Account{paid, free})

	counts := map[string]int{}
	const total = 100
	for i := 0; i < total; i++ {
		acc, _, err := nextAccountForModel("m", nil, "") // 空键 → 纯加权轮询
		if err != nil {
			t.Fatalf("第 %d 次选号失败: %v", i, err)
		}
		counts[acc.Path]++
	}
	ratio := float64(counts["paid.json"]) / float64(counts["free.json"])
	// 理论值 7000/3000 ≈ 2.33；留宽裕区间避免边界抖动误判。
	if ratio < 1.9 || ratio > 2.8 {
		t.Fatalf("加权比例偏离预期: paid=%d free=%d ratio=%.2f", counts["paid.json"], counts["free.json"], ratio)
	}
}

// QuotaTotal 有值时优先于付费标记作为权重。
func TestAccountWeightPrefersQuotaTotal(t *testing.T) {
	resetRoutingState(t)
	if got := accountWeight(testAccount("x.json", true, 12345)); got != 12345 {
		t.Fatalf("QuotaTotal 未优先: %d", got)
	}
	if got := accountWeight(testAccount("p.json", true, 0)); got != 7000 {
		t.Fatalf("付费号默认权重错误: %d", got)
	}
	if got := accountWeight(testAccount("f.json", false, 0)); got != 3000 {
		t.Fatalf("免费号默认权重错误: %d", got)
	}
	if got := accountWeight(nil); got != 1 {
		t.Fatalf("nil 账号权重应为 1: %d", got)
	}
}

// 账号池收缩后，已移除账号的加权计数与会话绑定都要被清理。
func TestPruneRoutingStateForRemovedAccounts(t *testing.T) {
	resetRoutingState(t)
	stickyBind("sc-x-keep", "keep.json", time.Now())
	stickyBind("sc-x-drop", "drop.json", time.Now())
	weightMu.Lock()
	weightCounters["keep.json"] = 5
	weightCounters["drop.json"] = 9
	weightMu.Unlock()

	alive := map[string]bool{"keep.json": true}
	pruneWeightCounters(alive)
	pruneStickyBindings(alive)

	weightMu.Lock()
	_, keptCounter := weightCounters["keep.json"]
	_, droppedCounter := weightCounters["drop.json"]
	weightMu.Unlock()
	if !keptCounter || droppedCounter {
		t.Fatalf("加权计数清理不正确: keep=%v drop=%v", keptCounter, droppedCounter)
	}
	if bound := stickyLookup("sc-x-keep", time.Now()); bound != "keep.json" {
		t.Fatalf("存活账号的绑定被误删: %q", bound)
	}
	if bound := stickyLookup("sc-x-drop", time.Now()); bound != "" {
		t.Fatalf("已移除账号的绑定未清理: %q", bound)
	}
}

// 48h 滚动窗口：越过软线的账号在选号阶段被软排除。
func TestWindowOverLimitExcludesAccountFromSelection(t *testing.T) {
	resetRoutingState(t)
	oldDisabled := windowDisabled
	windowDisabled = false
	t.Cleanup(func() { windowDisabled = oldDisabled })
	t.Cleanup(func() {
		tokenWindowMu.Lock()
		tokenWindows = map[string]*accountTokenWindow{}
		tokenWindowMu.Unlock()
	})

	a := testAccount("a.json", false, 0)
	b := testAccount("b.json", false, 0)
	withAccountPool(t, []*Account{a, b})

	// 把 a 的窗口灌到阈值之上
	tokenWindowMu.Lock()
	tokenWindows["a.json"] = &accountTokenWindow{
		Path:    "a.json",
		Edition: "cn",
		Buckets: []tokenBucket{{Hour: time.Now().Truncate(time.Hour).Unix(), Tokens: defaultWindowLimitCN + 1}},
	}
	tokenWindowMu.Unlock()

	if !windowOverLimit(a) {
		t.Fatal("a 应判定为越线")
	}
	if windowOverLimit(b) {
		t.Fatal("b 不该判定为越线")
	}
	for i := 0; i < 20; i++ {
		acc, _, err := nextAccountForModel("m", nil, "")
		if err != nil {
			t.Fatalf("选号失败: %v", err)
		}
		if acc.Path == "a.json" {
			t.Fatal("越线账号仍被选中")
		}
	}
}

// 全部账号都越线时必须回落到余量最大者，而不是直接失败。
func TestWindowAllOverLimitFallsBackToLargestHeadroom(t *testing.T) {
	resetRoutingState(t)
	oldDisabled := windowDisabled
	windowDisabled = false
	t.Cleanup(func() { windowDisabled = oldDisabled })
	t.Cleanup(func() {
		tokenWindowMu.Lock()
		tokenWindows = map[string]*accountTokenWindow{}
		tokenWindowMu.Unlock()
	})

	a := testAccount("a.json", false, 0)
	b := testAccount("b.json", false, 0)
	withAccountPool(t, []*Account{a, b})

	hour := time.Now().Truncate(time.Hour).Unix()
	tokenWindowMu.Lock()
	tokenWindows["a.json"] = &accountTokenWindow{Path: "a.json", Edition: "cn",
		Buckets: []tokenBucket{{Hour: hour, Tokens: defaultWindowLimitCN + 10_000_000}}}
	tokenWindows["b.json"] = &accountTokenWindow{Path: "b.json", Edition: "cn",
		Buckets: []tokenBucket{{Hour: hour, Tokens: defaultWindowLimitCN + 1_000_000}}}
	tokenWindowMu.Unlock()

	acc, _, err := nextAccountForModel("m", nil, "")
	if err != nil {
		t.Fatalf("全部越线时应回落到余量最大账号，实际报错: %v", err)
	}
	if acc.Path != "b.json" {
		t.Fatalf("应回落到余量更大的 b.json，实际 %s", acc.Path)
	}
}

// 窗口计数按小时分桶累加，且能落盘/读回。
func TestRecordAccountTokensAccumulatesAndPrunes(t *testing.T) {
	resetRoutingState(t)
	oldDisabled := windowDisabled
	windowDisabled = false
	t.Cleanup(func() { windowDisabled = oldDisabled })
	t.Cleanup(func() {
		tokenWindowMu.Lock()
		tokenWindows = map[string]*accountTokenWindow{}
		tokenWindowMu.Unlock()
	})

	acc := testAccount("a.json", false, 0)
	recordAccountTokens(acc, 1000)
	recordAccountTokens(acc, 500)
	total, limit := accountWindowUsage(acc)
	if total != 1500 {
		t.Fatalf("累计错误: %d", total)
	}
	if limit != defaultWindowLimitCN {
		t.Fatalf("阈值错误: %d", limit)
	}

	// 48 小时前的桶应被裁掉
	tokenWindowMu.Lock()
	old := time.Now().Truncate(time.Hour).Unix() - (tokenWindowHours+1)*3600
	tokenWindows["a.json"].Buckets = append(tokenWindows["a.json"].Buckets, tokenBucket{Hour: old, Tokens: 999999})
	pruneBucketsLocked(tokenWindows["a.json"], time.Now().Truncate(time.Hour).Unix())
	tokenWindowMu.Unlock()

	total, _ = accountWindowUsage(acc)
	if total != 1500 {
		t.Fatalf("过期桶未被裁掉: %d", total)
	}
}
