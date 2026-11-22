package main

// accountwindow.go —— 账号级 24 小时滚动 token 窗口 + 软阈值。
//
// 背景：workbuddy 上游对每个账号有滚动 24 小时的 token 配额。用满后返回
// code 6004，文案形如
//
//	usage exceeds frequency limit, but don't worry, your usage will reset at <时刻>
//
// 该冷却会跳到 6~30 小时后，无法通过重启网关清除（重置在上游服务器侧）。
//
// 实测口径（关键）：对 25 次 CN 429 与 31 次 INTL 429，取 [reset-24h, 触发]
// 这个区间去量，窗口内用量分别稳定在
//
//	国内站账号（体验版）  200M（均值 200.4M，标准差 0.98M，20/25 落在 199~201M）
//	国际站免费账号        100M（逐个落在 100.0~102.7M）
//
// 所以窗口是 24 小时、不是 48 小时；阈值是 200M / 100M、不是 175M / 85M。
// 早期按 48 小时切片得到 400M/450M 的「交替值」，那是把上个周期的用量混了进来。
//
// 本文件按小时分桶累计每个账号的 token 消耗，维护 24+1 个桶的滚动窗口；当窗口内
// 累计值达到软阈值（实测值的 95%：190M / 95M）时，该账号在选号阶段被**软排除**：
// 优先用其他账号；只有全部账号都越过软线时才回落到余量最大的账号。这样既能「尽量
// 吃满」每个账号的额度，又避免撞到上游硬墙后长时间不可用。
//
// 付费账号的限流规则未知，不猜值、不纳入软限（只靠 429 被动冷却）。
//
// 阈值来源（环境变量，容器启动时用 -e 指定）：
//
//	WBGW_WINDOW_LIMIT_INTL  国际站账号的 24h 软阈值，默认 95000000
//	WBGW_WINDOW_LIMIT_CN    国内站账号的 24h 软阈值，默认 190000000
//	WBGW_WINDOW_DISABLED    置 1 关闭本机制
//
// 计数含缓存命中（与上游 usage.total_tokens 同口径），因此是保守估计。

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// 窗口与阈值来自实测：对 25 次 CN 429 与 31 次 INTL 429，用 [reset-24h, 触发]
	// 这个区间去量，用量分别稳定在 200M / 100M（标准差 <1M）。
	// 原先写的 48 小时 / 175M / 85M 三个参数都不对。
	tokenWindowHours     = 24
	tokenWindowBucketCap = tokenWindowHours + 2 // 当前小时 + 24 个历史小时 + 余量
	tokenWindowSaveEvery = 10 * time.Second
)

const (
	// 软阈值取实测值的 95%：CN 200M→190M、INTL 100M→95M。
	// 留 5%~10M 余量（约 10~20 个请求）够调度层切走，又不至于浪费额度。
	defaultWindowLimitIntl = int64(95_000_000)
	defaultWindowLimitCN   = int64(190_000_000)
)

type tokenBucket struct {
	Hour   int64 `json:"hour"` // Unix 小时（UTC）
	Tokens int64 `json:"tokens"`
	// Model 是该桶归属的模型（归一化后）。空串表示旧格式的账号级合计桶。
	Model string `json:"model,omitempty"`
}

// accountTokenWindow 是单个账号的滚动窗口。
//
// ModelBuckets 按模型分开计数：上游 6004 的语义是按模型限流（实测 10 次触发点，
// 同账号其它模型用量都是 0.0M，触发时永远是当前模型自己到 200M）。若把所有模型
// 合并计数，跑一个小模型就会平白吃掉大模型的软限预算，让大模型提前被软排除，
// 反而加速撞硬墙。Buckets 只用于兼容旧落盘文件，读入时迁移进 ModelBuckets。
type accountTokenWindow struct {
	Path         string         `json:"path"`
	Edition      string         `json:"edition"`
	Buckets      []tokenBucket  `json:"buckets,omitempty"` // 旧格式：账号级合计，已废弃
	ModelBuckets map[string][]tokenBucket `json:"modelBuckets,omitempty"`
}

type tokenWindowFileFormat struct {
	Schema  int                            `json:"schema"`
	SavedAt int64                          `json:"savedAt"`
	Windows map[string]*accountTokenWindow `json:"windows"`
}

var (
	tokenWindowMu    sync.Mutex
	tokenWindows     = map[string]*accountTokenWindow{}
	tokenWindowReady bool
	tokenWindowLast  time.Time

	windowLimitIntl = defaultWindowLimitIntl
	windowLimitCN   = defaultWindowLimitCN

	// 付费账号的限流规则未知：不做猜测，直接不纳入软限。
	windowDisabled bool

	weightMu       sync.Mutex
	weightCounters = map[string]int64{}
)

// initTokenWindow 读取环境变量阈值并加载落盘窗口。
func initTokenWindow() {
	if v := strings.TrimSpace(os.Getenv("WBGW_WINDOW_DISABLED")); v == "1" || strings.EqualFold(v, "true") {
		windowDisabled = true
	}
	if v := strings.TrimSpace(os.Getenv("WBGW_WINDOW_LIMIT_INTL")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			windowLimitIntl = n
		} else {
			log.Printf("[TokenWindow] WBGW_WINDOW_LIMIT_INTL=%q 无法解析，沿用默认 %d", v, windowLimitIntl)
		}
	}
	if v := strings.TrimSpace(os.Getenv("WBGW_WINDOW_LIMIT_CN")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			windowLimitCN = n
		} else {
			log.Printf("[TokenWindow] WBGW_WINDOW_LIMIT_CN=%q 无法解析，沿用默认 %d", v, windowLimitCN)
		}
	}
	tokenWindowMu.Lock()
	loadTokenWindowsLocked()
	tokenWindowReady = true
	tokenWindowMu.Unlock()
	if windowDisabled {
		log.Printf("[TokenWindow] 已按 WBGW_WINDOW_DISABLED 关闭账号级 token 窗口软限")
		return
	}
	log.Printf("[TokenWindow] 账号级 %d 小时滚动 token 软限已启用：国际站=%d 国内站=%d 落盘=%s",
		tokenWindowHours, windowLimitIntl, windowLimitCN, tokenWindowPath())
}

func tokenWindowPath() string {
	dir := "."
	if cfg.AuthDir != "" {
		dir = cfg.AuthDir
	} else if cfg.AuthFile != "" {
		dir = filepath.Dir(cfg.AuthFile)
	}
	return filepath.Join(dir, "account_tokens.json")
}

func loadTokenWindowsLocked() {
	data, err := os.ReadFile(tokenWindowPath())
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[TokenWindow] 读取 %s 失败: %v", tokenWindowPath(), err)
		}
		return
	}
	var doc tokenWindowFileFormat
	if err := json.Unmarshal(data, &doc); err != nil {
		log.Printf("[TokenWindow] 解析 %s 失败: %v（按空窗口启动）", tokenWindowPath(), err)
		return
	}
	now := time.Now()
	hour := now.Truncate(time.Hour).Unix()
	kept := 0
	for path, w := range doc.Windows {
		if w == nil {
			continue
		}
		pruneBucketsLocked(w, hour)
		if len(w.Buckets) == 0 {
			continue
		}
		w.Path = path
		tokenWindows[path] = w
		kept++
	}
	if kept > 0 {
		log.Printf("[TokenWindow] 已载入 %d 个账号的滚动窗口（%s）", kept, tokenWindowPath())
	}
}

// pruneBucketsLocked 裁掉滑出 24h 窗口的桶，并迁移旧格式（账号级合计）。
func pruneBucketsLocked(w *accountTokenWindow, hour int64) {
	// hour 是 Unix 小时戳（秒），窗口长度必须换算成秒：tokenWindowHours*3600。
	// 这里曾经写成 hour - tokenWindowHours，等于只减 48 秒，
	// 于是 24 小时窗口退化成「当前这一小时」，软限几乎永不触发（见 window_prune_test.go）。
	cut := hour - tokenWindowHours*3600
	if w.ModelBuckets == nil {
		w.ModelBuckets = make(map[string][]tokenBucket)
	}
	// 旧文件只有账号级 Buckets：没有模型维度，无法拆分，只能整体丢弃。
	// 保留会与按模型计数叠加（双计），丢掉的代价仅是一天的窗口从零重算。
	if len(w.Buckets) > 0 {
		log.Printf("[TokenWindow] 账号 %s 的窗口是旧格式（账号级合计，无模型维度），已丢弃重算", w.Path)
		w.Buckets = nil
	}
	for model, buckets := range w.ModelBuckets {
		out := buckets[:0]
		for _, b := range buckets {
			if b.Hour > cut && b.Tokens > 0 {
				out = append(out, b)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Hour < out[j].Hour })
		if len(out) > tokenWindowBucketCap {
			out = out[len(out)-tokenWindowBucketCap:]
		}
		if len(out) == 0 {
			delete(w.ModelBuckets, model)
			continue
		}
		w.ModelBuckets[model] = out
	}
}

// accountEditionOf 取账号站点标识（cn / intl），不依赖 accountMu。
func accountEditionOf(acc *Account) string {
	if acc == nil {
		return ""
	}
	if acc.Auth != nil && acc.Auth.Edition != "" {
		return strings.ToLower(acc.Auth.Edition)
	}
	return strings.ToLower(acc.Edition)
}

// accountTokenLimit 返回该账号的 24h 软阈值。
//
// 实测口径：CN 账号每 24 小时 200M、INTL 每 24 小时 100M，超了上游回 429 code 6004。
// 软阈值取实测值的 95%（190M / 95M）留出调度切换的余量。
//
// 付费账号：限流规则未知。不猜一个值上去，直接返回 0 = 不纳入软限，
// 靠 429 反馈被动冷却。等有实测样本再补。
// accountTokenLimit 返回软阈值（实测硬墙的 95%）。
func accountTokenLimit(acc *Account) int64 {
	if windowDisabled || acc == nil {
		return 0
	}
	if acc.IsPaidUser || (acc.PlanLabel != "" && !strings.Contains(acc.PlanLabel, "免费") && !strings.Contains(acc.PlanLabel, "体验")) {
		return 0
	}
	switch accountEditionOf(acc) {
	case "intl", "global", "international":
		return windowLimitIntl
	default:
		return windowLimitCN
	}
}

// accountTokenHardLimit 返回该账号的硬上限估计（软线 / 0.95，即实测硬墙本身）。
//
// 软线定在实测值的 95%，留出的 5% 是缓冲：越线后仍可放行少量请求，
// 但一旦逼近硬墙就必须彻底停用，不能继续放行 —— 否则缓冲被吃光，
// 必然撞 6004，换来 16~30 小时的模型冷却，代价远大于那 5% 的额度。
func accountTokenHardLimit(acc *Account) int64 {
	soft := accountTokenLimit(acc)
	if soft <= 0 {
		return 0
	}
	// soft 是实测硬墙的 95%：反推硬墙。
	return soft * 100 / 95
}

// windowHardExceeded 判断该账号在该模型上是否已越过实测硬墙。
//
// 注意：这只用于**排序和告警**，不是拒绝服务的依据。服务可用性优先：
// 达到硬墙的账号仍会被选中（排在全部分账号之后），因为「拒绝请求」是 100% 失败，
// 而撞 6004 只冷却「该账号 × 该模型」这一个组合，其他账号和模型照常可用。
func windowHardExceeded(acc *Account, model string) bool {
	hard := accountTokenHardLimit(acc)
	if hard <= 0 {
		return false
	}
	total, _ := accountWindowUsage(acc, model)
	return total >= hard
}

// tokenCountOf 从上游 usage 取总 token（含缓存命中），取不到返回 0。
func tokenCountOf(usage map[string]any) int64 {
	if n, ok := usageTotalTokens(usage); ok && n > 0 {
		return n
	}
	return 0
}

// accountWeight 返回该账号在负载均衡里的权重。
// 付费账号月积分 7000（3000 签到 + 4000 套餐），免费账号 3000，权重按积分比例分配。
//
// Deprecated: 这是「按周期总额度」分配的历史口径。总额大的账号被派得更多，
// 但总额不反映还剩多少，结果是总额高、余额少的账号最先被打空（2026-11 cn 账号实测）。
// 调度一律走 accountPriorityWeight。
func accountWeight(acc *Account) int64 {
	if acc == nil {
		return 1
	}
	if acc.QuotaTotal > 0 {
		return int64(acc.QuotaTotal)
	}
	if acc.IsPaidUser {
		return 7000
	}
	return 3000
}

const (
	// 权重下限/上限：避免 float 极端值把加权轮询压死或让某号独占。
	priorityWeightFloor = 1e-4
	priorityWeightCeil  = 1e6
	// 到期望远镜：没有到期信息的账号按「30 天后到期」参与计算，
	// 让它既不抢被快过期的号，也不会被永久饿死。
	priorityDefaultHorizon = 30 * 24 * time.Hour
	// 到期不足 1 小时按 1 小时算，防止 R/T 爆炸。
	priorityMinHorizon = time.Hour
)

// accountPriorityWeight 返回账号的调度权重（浮点，越大分到的请求越多）。
//
// 目标是两个约束同时成立：
//  1. 没有积分被浪费 —— 快过期的积分必须先烧掉；
//  2. 没有账号率先耗尽 —— 各账号的「余额消耗速率」应当与其可用时间成正比。
//
// 为此权重取「余额 / 距到期小时数」，即该账号**必须的燃烧速率**。
// 按这个速率分配流量时，若总运力足够，所有账号会在**各自到期时刻**同时归零，
// 既不会有人提前打空，也不会有余额过期作废；运力不足时，最紧迫的号仍然优先。
//
// 这与旧的「按 QuotaTotal 加权」有本质区别：旧口径只看周期总额，不看剩多少，
// 于是总额最大的号被派最多请求、最先被打空 —— 正是 cn 账号被用光的原因。
func accountPriorityWeight(acc *Account, now time.Time) float64 {
	return accountPriorityWeightForModel(acc, "", now)
}

// accountPriorityWeightForModel 是 accountPriorityWeight 的带模型版本。
//
// 软线惩罚必须按模型判定：上游 6004 只封当前模型，账号在别的模型上还有额度时
// 不该被压到 1% 权重。model 为空时只按账号余额计算（用于报表展示）。
func accountPriorityWeightForModel(acc *Account, model string, now time.Time) float64 {
	if acc == nil {
		return priorityWeightFloor
	}
	// 余额优先取积分包汇总；拿不到积分包（企业号/旧快照/接口失败）时退到个人周期剩余。
	remain := acc.CreditRemain
	haveGrants := acc.CreditGrants > 0
	if !haveGrants {
		if acc.QuotaUnlimited {
			return priorityWeightFloor // 不限量：不参与紧张度竞争，靠轮询摊平
		}
		remain = acc.QuotaRemaining
	} else if acc.QuotaRemaining >= 0 && acc.QuotaKnown && acc.QuotaRemaining < remain {
		// 两个口径不一致时取小：国际站的资源包 CapacityRemain 实测不随消耗回落
		// （包显示 100，而周期摘要剩余只有 42.76），以较保守的一方为准，
		// 避免高估余额把流量派给快要耗尽的账号。
		remain = acc.QuotaRemaining
	}
	if remain <= 0 {
		// 没有可用余额：给一个极小的权重，让它只在别的号都不可用时才被选中。
		return priorityWeightFloor
	}
	horizon := priorityDefaultHorizon
	if haveGrants && acc.CreditExpireAt > 0 {
		horizon = time.Unix(acc.CreditExpireAt, 0).Sub(now)
		if horizon < priorityMinHorizon {
			horizon = priorityMinHorizon
		}
	}
	w := remain / horizon.Hours()
	// 已经越过 24h 软线的账号压到最低：它再吃流量就会撞上游硬墙，
	// 而硬墙冷却 16~30 小时，比等它自然过期的代价大得多。
	// 按模型判定：软线是按模型计的，其它模型还有额度时不该被连坐。
	// model 为空（报表展示）时取窗口内最吃紧的模型来代表该账号状态。
	if model != "" {
		if windowOverLimit(acc, model) {
			w *= 0.01
		}
	}
	if w < priorityWeightFloor {
		w = priorityWeightFloor
	}
	if w > priorityWeightCeil {
		w = priorityWeightCeil
	}
	return w
}

// weightScore 返回该账号的「虚拟完成时间」，值最小者优先（加权轮询）。
func weightScore(path string, weight float64) float64 {
	if weight <= 0 {
		weight = 1
	}
	weightMu.Lock()
	c := weightCounters[path]
	weightMu.Unlock()
	return float64(c) / weight
}

// pruneWeightCounters 删除已不在账号池中的路径计数。
// 账号池会热加载（凭据增删），已移除账号的累计计数若不清理，会随进程寿命无限增长；
// 同名路径重新出现时也会继承旧计数，导致加权轮询起点偏移。
func pruneWeightCounters(alive map[string]bool) {
	weightMu.Lock()
	for path := range weightCounters {
		if !alive[path] {
			delete(weightCounters, path)
		}
	}
	weightMu.Unlock()
}

// bumpWeight 记录一次实际调度，推进该账号的加权轮询计数。
func bumpWeight(acc *Account) {
	if acc == nil {
		return
	}
	weightMu.Lock()
	weightCounters[acc.Path]++
	weightMu.Unlock()
}

// recordAccountTokens 把一次成功响应的 token 数计入该账号在该模型上的滚动窗口。
//
// 必须带 model：上游 6004 是按模型限流的，实测 10 次触发点的同账号「其它模型」
// 用量都是 0.0M——触发时永远是当前模型自己到 200M。按账号合并计数会让小模型
// 吃掉大模型的软限预算，使大模型提前被软排除。
func recordAccountTokens(acc *Account, model string, tokens int64) {
	if windowDisabled || acc == nil || tokens <= 0 {
		return
	}
	model = normalizeModelName(model)
	if model == "" {
		return
	}
	hour := time.Now().Truncate(time.Hour).Unix()
	tokenWindowMu.Lock()
	w := tokenWindows[acc.Path]
	if w == nil {
		w = &accountTokenWindow{Path: acc.Path, Edition: accountEditionOf(acc)}
		tokenWindows[acc.Path] = w
	}
	if w.Edition == "" {
		w.Edition = accountEditionOf(acc)
	}
	pruneBucketsLocked(w, hour)
	found := false
	buckets := w.ModelBuckets[model]
	for i := range buckets {
		if buckets[i].Hour == hour {
			buckets[i].Tokens += tokens
			found = true
			break
		}
	}
	if !found {
		buckets = append(buckets, tokenBucket{Hour: hour, Tokens: tokens, Model: model})
	}
	w.ModelBuckets[model] = buckets
	total := windowTotalLocked(w, model)
	limit := accountTokenLimit(acc)
	needSave := time.Since(tokenWindowLast) > tokenWindowSaveEvery
	if needSave {
		tokenWindowLast = time.Now()
	}
	if limit > 0 && total >= limit {
		log.Printf("[TokenWindow] 账号 %s 站点=%s 48小时累计=%d 阈值=%d 已越过软线，后续请求优先改用其他账号",
			filepath.Base(acc.Path), w.Edition, total, limit)
	}
	tokenWindowMu.Unlock()
	if needSave {
		persistTokenWindows()
	}
}

// windowTotalLocked 返回该账号在指定模型上的 24h 累计 token。
func windowTotalLocked(w *accountTokenWindow, model string) int64 {
	if w == nil {
		return 0
	}
	var sum int64
	for _, b := range w.ModelBuckets[model] {
		sum += b.Tokens
	}
	return sum
}

// windowMaxModelLocked 返回该账号窗口内用量最大的模型及其累计量。
// 用于日志与「该账号还剩多少额度」的判断：按模型限流时，账号的可服务能力
// 由最吃紧的那个模型决定，而不是所有模型加总。
func windowMaxModelLocked(w *accountTokenWindow) (model string, total int64) {
	if w == nil {
		return "", 0
	}
	for m, buckets := range w.ModelBuckets {
		var s int64
		for _, b := range buckets {
			s += b.Tokens
		}
		if s > total {
			model, total = m, s
		}
	}
	return model, total
}

// accountWindowUsage 返回该账号该模型当前 24h 窗口内的累计 token 与阈值。
func accountWindowUsage(acc *Account, model string) (total int64, limit int64) {
	if windowDisabled || acc == nil {
		return 0, 0
	}
	limit = accountTokenLimit(acc)
	hour := time.Now().Truncate(time.Hour).Unix()
	tokenWindowMu.Lock()
	w := tokenWindows[acc.Path]
	if w == nil {
		tokenWindowMu.Unlock()
		return 0, limit
	}
	pruneBucketsLocked(w, hour)
	total = windowTotalLocked(w, normalizeModelName(model))
	tokenWindowMu.Unlock()
	return total, limit
}

// windowOverLimit 判断该账号在指定模型上是否已越过 24h 软阈值。
func windowOverLimit(acc *Account, model string) bool {
	total, limit := accountWindowUsage(acc, model)
	return limit > 0 && total >= limit
}

// windowHeadroom 返回该账号该模型剩余余量（用于「全部越线时挑余量最大者」的保底选择）。
func windowHeadroom(acc *Account, model string) int64 {
	total, limit := accountWindowUsage(acc, model)
	if limit <= 0 {
		return 1 << 62
	}
	return limit - total
}

func persistTokenWindows() {
	tokenWindowMu.Lock()
	doc := tokenWindowFileFormat{Schema: 1, SavedAt: time.Now().Unix(), Windows: tokenWindows}
	data, err := json.MarshalIndent(doc, "", "  ")
	path := tokenWindowPath()
	tokenWindowMu.Unlock()
	if err != nil {
		log.Printf("[TokenWindow] 序列化失败: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		log.Printf("[TokenWindow] 写入 %s 失败: %v", path, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("[TokenWindow] 落盘 %s 失败: %v", path, err)
	}
}

// tokenWindowGC 周期清理：删除已滑出窗口且无数据的账号条目并落盘。
func tokenWindowGC() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		hour := time.Now().Truncate(time.Hour).Unix()
		tokenWindowMu.Lock()
		for path, w := range tokenWindows {
			pruneBucketsLocked(w, hour)
			if len(w.ModelBuckets) == 0 {
				delete(tokenWindows, path)
			}
		}
		tokenWindowMu.Unlock()
		persistTokenWindows()
	}
}

// formatTokenWindowLine 生成监控/日志用的一行摘要（取窗口内最吃紧的模型）。
func formatTokenWindowLine(acc *Account, model string) string {
	if model == "" {
		// 调用方只想知道整体状态时，报告用量最大的模型。
		hour := time.Now().Truncate(time.Hour).Unix()
		tokenWindowMu.Lock()
		w := tokenWindows[acc.Path]
		if w != nil {
			pruneBucketsLocked(w, hour)
			model, _ = windowMaxModelLocked(w)
		}
		tokenWindowMu.Unlock()
		if model == "" {
			total, limit := accountWindowUsage(acc, model)
			if limit <= 0 {
				return ""
			}
			return fmt.Sprintf("%.1f%%(%d/%d)", float64(total)/float64(limit)*100, total, limit)
		}
	}
	total, limit := accountWindowUsage(acc, model)
	if limit <= 0 {
		return ""
	}
	pct := float64(total) / float64(limit) * 100
	return fmt.Sprintf("%s %.1f%%(%d/%d)", model, pct, total, limit)
}
