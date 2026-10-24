package main

// sticky.go —— 会话粘性（session affinity）路由。
//
// 为什么需要：上游（DeepSeek 系）按**账号**维护前缀缓存（prompt cache）。实测同一
// 前缀连续两次请求，命中时 prompt_cache_hit_tokens 占比 99.7%，credit 从 0.88 降到
// 0.03（约 29 倍差价）；未命中则全额计费。而本网关的自营调度是纯轮询
// （nextAccountForModel 里 rrIndex 逐格推进），同一会话的多轮请求会被轮流分配到
// 5 个账号上，每个账号的缓存都命中不了 —— 5 个账号轮询把命中率压到约 1/5。
//
// 做法：从请求体派生一个稳定的「会话键」，把该会话绑定到某个账号；绑定期间同一
// 会话的请求都走同一账号，让上游前缀缓存持续命中。绑定带 TTL 滚动续期，账号不可
// 用（冷却 / 越线 / 授权失效）时自动解绑并重新分配。
//
// 会话键来源（按优先级）：
//  1. 顶层 conversation_id / conversationId / session_id（部分客户端会带）
//  2. metadata.conversation_id / metadata.conversationId / metadata.user_id
//  3. 内容派生回退：全部 system/developer 消息 + 第一条 user 消息的规范化文本
//     —— 通用 OpenAI 客户端（含本机 Hermes）不发会话标识，但它每轮都会重发完整
//     历史，因此「系统提示 + 最早的用户提问」在多轮之间保持稳定，可作为会话指纹。
//
// 环境变量：
//
//	WBGW_STICKY_DISABLED  置 1 关闭会话粘性（回到纯轮询）
//	WBGW_STICKY_TTL       绑定存活时长，默认 30m（Go duration 格式，如 1h、45m）

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	stickyKeyPrefix    = "sc-"
	stickyDefaultTTL   = 30 * time.Minute
	stickyContentLimit = 3000 // 单条消息参与哈希的最大字符数
)

type stickyBinding struct {
	AccountPath string    `json:"account_path"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
}

var (
	stickyMu      sync.Mutex
	stickyBinds   = map[string]*stickyBinding{}
	stickyTTL     = stickyDefaultTTL
	stickyDisable bool
)

func initSticky() {
	if v := strings.TrimSpace(os.Getenv("WBGW_STICKY_DISABLED")); v == "1" || strings.EqualFold(v, "true") {
		stickyDisable = true
	}
	if v := strings.TrimSpace(os.Getenv("WBGW_STICKY_TTL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			stickyTTL = d
		} else {
			log.Printf("[Sticky] WBGW_STICKY_TTL=%q 无法解析，沿用默认 %s", v, stickyTTL)
		}
	}
	if stickyDisable {
		log.Printf("[Sticky] 会话粘性已按 WBGW_STICKY_DISABLED 关闭，调度回到纯轮询")
		return
	}
	log.Printf("[Sticky] 会话粘性已启用：TTL=%s（同一会话固定同一账号，让上游前缀缓存持续命中）", stickyTTL)
}

// sessionKeyFromBody 从请求体派生会话键；无法派生时返回空串（退回纯轮询）。
func sessionKeyFromBody(raw []byte) string {
	if stickyDisable || len(raw) == 0 {
		return ""
	}
	var doc struct {
		ConversationID  string `json:"conversation_id"`
		ConversationID2 string `json:"conversationId"`
		SessionID       string `json:"session_id"`
		Metadata        struct {
			ConversationID  string `json:"conversation_id"`
			ConversationID2 string `json:"conversationId"`
			UserID          string `json:"user_id"`
		} `json:"metadata"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		System json.RawMessage `json:"system"`
		Input  json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	for _, explicit := range []string{
		doc.ConversationID, doc.ConversationID2, doc.SessionID,
		doc.Metadata.ConversationID, doc.Metadata.ConversationID2, doc.Metadata.UserID,
	} {
		if s := strings.TrimSpace(explicit); s != "" {
			return stickyKeyPrefix + "x" + shortHash(s)
		}
	}

	var parts []string
	if len(doc.System) > 0 {
		parts = append(parts, "sys:"+canonicalText(doc.System))
	}
	gotUser := false
	for _, m := range doc.Messages {
		role := strings.ToLower(m.Role)
		switch {
		case role == "system" || role == "developer":
			parts = append(parts, "sys:"+canonicalText(m.Content))
		case role == "user" && !gotUser:
			parts = append(parts, "u0:"+canonicalText(m.Content))
			gotUser = true
		}
		if gotUser {
			break
		}
	}
	if len(doc.Input) > 0 {
		parts = append(parts, "in:"+canonicalText(doc.Input))
	}
	if len(parts) == 0 {
		return ""
	}
	return stickyKeyPrefix + "d" + shortHash(strings.Join(parts, "\n"))
}

// canonicalText 把消息内容归一化成稳定字符串（字符串 / 内容块数组 / 其它）。
func canonicalText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if s[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) == nil {
			return limitRunes(str, stickyContentLimit)
		}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil && len(blocks) > 0 {
		var b strings.Builder
		for _, bl := range blocks {
			if bl.Text != "" {
				b.WriteString(bl.Text)
			}
		}
		if b.Len() > 0 {
			return limitRunes(b.String(), stickyContentLimit)
		}
	}
	return limitRunes(s, stickyContentLimit/4)
}

func limitRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	// 按字节上限截断后回退到最后一个完整 UTF-8 字符边界：
	// 直接切 s[:n] 会把多字节汉字劈成半个，哈希输入不稳定且日志里是乱码。
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func shortHash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// stickyLookup 返回该会话当前绑定的账号路径；无绑定或已过期返回空串。
func stickyLookup(key string, now time.Time) string {
	if stickyDisable || key == "" {
		return ""
	}
	stickyMu.Lock()
	defer stickyMu.Unlock()
	b := stickyBinds[key]
	if b == nil {
		return ""
	}
	if now.After(b.ExpiresAt) {
		delete(stickyBinds, key)
		return ""
	}
	return b.AccountPath
}

// stickyBind 绑定 / 续期；成功请求后调用。
func stickyBind(key, accountPath string, now time.Time) {
	if stickyDisable || key == "" || accountPath == "" {
		return
	}
	stickyMu.Lock()
	defer stickyMu.Unlock()
	b := stickyBinds[key]
	if b == nil {
		b = &stickyBinding{AccountPath: accountPath, CreatedAt: now}
		stickyBinds[key] = b
	}
	b.AccountPath = accountPath
	b.ExpiresAt = now.Add(stickyTTL)
}

// stickyUnbind 解除绑定（账号不可用或请求失败时调用）。
func stickyUnbind(key string) {
	if stickyDisable || key == "" {
		return
	}
	stickyMu.Lock()
	delete(stickyBinds, key)
	stickyMu.Unlock()
}

// pruneStickyBindings 删除指向已不在账号池中的账号的绑定。
// 账号池热加载后，指向已删除账号的绑定永远命不中，只会白占内存到 TTL 到期；
// 而账号池被测试或重启替换时，旧绑定还会把请求错绑到同名的旧账号路径上。
func pruneStickyBindings(alive map[string]bool) {
	if stickyDisable {
		return
	}
	stickyMu.Lock()
	for k, b := range stickyBinds {
		if b == nil || !alive[b.AccountPath] {
			delete(stickyBinds, k)
		}
	}
	stickyMu.Unlock()
}

// stickyReset 清空全部绑定与权重计数，仅供测试隔离状态使用。
func stickyReset() {
	stickyMu.Lock()
	stickyBinds = map[string]*stickyBinding{}
	stickyMu.Unlock()
	weightMu.Lock()
	weightCounters = map[string]int64{}
	weightMu.Unlock()
}

// stickyGC 周期清理过期绑定。
func stickyGC() {
	if stickyDisable {
		return
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for now := range ticker.C {
		stickyMu.Lock()
		for k, b := range stickyBinds {
			if now.After(b.ExpiresAt) {
				delete(stickyBinds, k)
			}
		}
		stickyMu.Unlock()
	}
}

// stickyStats 返回当前绑定数与启停状态，供日志/监控使用。
func stickyStats() (int, bool, time.Duration) {
	stickyMu.Lock()
	n := len(stickyBinds)
	stickyMu.Unlock()
	return n, !stickyDisable, stickyTTL
}
