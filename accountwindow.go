package main

// accountwindow.go —— 账号级 48 小时滚动 token 窗口 + 软阈值。
//
// 背景（2026-10 实测）：workbuddy 上游对每个账号有滚动 48 小时的 token 配额。
// 用满后返回 code 6004，文案形如
//
//	usage exceeds frequency limit, but don't worry, your usage will reset at <时刻>
//
// 并且该冷却会跳到 16~30 小时后，无法通过重启网关清除（重置在上游服务器侧）。
// 实测等效上限：
//
//	国际站免费账号（额度 100）      ≈ 1.01 亿 token（100.9M / 101.0M 两个账号吻合）
//	国内站 2200/2900 额度账号       ≈ 2.1 亿 token（209.2M / 209.5M 吻合）
//	国内站 4500 额度账号            ≈ 2.4 亿 token（239.4M）
//
// 本文件按小时分桶累计每个账号的 token 消耗，维护 48+1 个桶的滚动窗口；当窗口内
// 累计值达到阈值（默认取实测值的 ~85%，留安全边际）时，该账号在选号阶段被**软排除**：
// 优先用其他账号；只有全部账号都越过软线时才回落到余量最大的账号。这样既能「尽量
// 吃满」每个账号的额度，又避免撞到上游硬墙后长时间（16~30 小时）不可用。
//
// 阈值来源（环境变量，容器启动时用 -e 指定）：
//
//	WBGW_WINDOW_LIMIT_INTL  国际站账号的 48h 软阈值，默认 85000000
//	WBGW_WINDOW_LIMIT_CN    国内站账号的 48h 软阈值，默认 175000000
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
	tokenWindowHours     = 48
	tokenWindowBucketCap = tokenWindowHours + 2 // 当前小时 + 48 个历史小时 + 余量
	tokenWindowSaveEvery = 10 * time.Second
)

const (
	defaultWindowLimitIntl = int64(85_000_000)
	defaultWindowLimitCN   = int64(175_000_000)
	// 付费账号额度约为免费账号的 7/3（免费=签到 3000 积分，付费=3000+4000=7000），
	// 阈值按同一比例放大：免费 175M/85M → 付费 400M/200M。
	defaultWindowLimitIntlPaid = int64(200_000_000)
	defaultWindowLimitCNPaid   = int64(400_000_000)
)

type tokenBucket struct {
	Hour   int64 `json:"hour"` // Unix 小时（UTC）
	Tokens int64 `json:"tokens"`
}

type accountTokenWindow struct {
	Path    string        `json:"path"`
	Edition string        `json:"edition"`
	Buckets []tokenBucket `json:"buckets"`
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

	windowLimitIntlPaid = defaultWindowLimitIntlPaid
	windowLimitCNPaid   = defaultWindowLimitCNPaid
	windowDisabled      bool

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

func pruneBucketsLocked(w *accountTokenWindow, hour int64) {
	cut := hour - tokenWindowHours
	out := w.Buckets[:0]
	for _, b := range w.Buckets {
		if b.Hour > cut && b.Tokens > 0 {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hour < out[j].Hour })
	if len(out) > tokenWindowBucketCap {
		out = out[len(out)-tokenWindowBucketCap:]
	}
	w.Buckets = out
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

// accountTokenLimit 返回该账号的 48h 软阈值。
func accountTokenLimit(acc *Account) int64 {
	if windowDisabled {
		return 0
	}
	intl := accountEditionOf(acc) == "intl" || accountEditionOf(acc) == "global" || accountEditionOf(acc) == "international"
	if acc.IsPaidUser || acc.PlanLabel != "" && !strings.Contains(acc.PlanLabel, "免费") && !strings.Contains(acc.PlanLabel, "体验") {
		if intl {
			return windowLimitIntlPaid
		}
		return windowLimitCNPaid
	}
	if intl {
		return windowLimitIntl
	}
	return windowLimitCN
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

// weightScore 返回该账号的「虚拟完成时间」，值最小者优先（加权轮询）。
func weightScore(path string, weight int64) float64 {
	if weight <= 0 {
		weight = 1
	}
	weightMu.Lock()
	c := weightCounters[path]
	weightMu.Unlock()
	return float64(c) / float64(weight)
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

// recordAccountTokens 把一次成功响应的 token 数计入该账号的滚动窗口。
func recordAccountTokens(acc *Account, tokens int64) {
	if windowDisabled || acc == nil || tokens <= 0 {
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
	for i := range w.Buckets {
		if w.Buckets[i].Hour == hour {
			w.Buckets[i].Tokens += tokens
			found = true
			break
		}
	}
	if !found {
		w.Buckets = append(w.Buckets, tokenBucket{Hour: hour, Tokens: tokens})
	}
	total := windowTotalLocked(w)
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

func windowTotalLocked(w *accountTokenWindow) int64 {
	var sum int64
	for _, b := range w.Buckets {
		sum += b.Tokens
	}
	return sum
}

// accountWindowUsage 返回该账号当前 48h 窗口内的累计 token 与阈值。
func accountWindowUsage(acc *Account) (total int64, limit int64) {
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
	total = windowTotalLocked(w)
	tokenWindowMu.Unlock()
	return total, limit
}

// windowOverLimit 判断该账号是否已越过 48h 软阈值。
func windowOverLimit(acc *Account) bool {
	total, limit := accountWindowUsage(acc)
	return limit > 0 && total >= limit
}

// windowHeadroom 返回剩余余量（用于「全部越线时挑余量最大者」的保底选择）。
func windowHeadroom(acc *Account) int64 {
	total, limit := accountWindowUsage(acc)
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
			if len(w.Buckets) == 0 {
				delete(tokenWindows, path)
			}
		}
		tokenWindowMu.Unlock()
		persistTokenWindows()
	}
}

// formatTokenWindowLine 生成监控/日志用的一行摘要。
func formatTokenWindowLine(acc *Account) string {
	total, limit := accountWindowUsage(acc)
	if limit <= 0 {
		return ""
	}
	pct := float64(total) / float64(limit) * 100
	return fmt.Sprintf("%.1f%%(%d/%d)", pct, total, limit)
}
