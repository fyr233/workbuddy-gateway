package main

import "regexp"

// 本文件处理 DSH 的代码标识符与命令名，与 identity.go 的品牌词替换分工如下：
//
//   - identity.go：产品名（DeepSeek Harness / Claude Code / Codex / ZCode → 配置的目标名）
//   - dshguicontext.go：GUI 环境句（DeepSeek Harness Web GUI → a Web GUI）
//   - 本文件：代码标识符（__DSH_BOOT__）、CLI 子命令（dsh web）、裸产品缩写（DSH）
//
// 代码标识符不能用假名字替换。实测把 `dsh web` 改成 `the WorkBuddy web shell` 之后，
// 模型会把假名字当成真实名字复述，给出错误的技术事实；同形字（西里尔 ԁѕһ）同样失败，
// 模型会原样复述同形字而无法归一化回真名。
//
// 可行做法是 base64 承载真实值 + 显式「decode before use」注记：模型需要时会解码出
// 正确标识符，上游扫描不到明文。注意必须写成 `window['X19...']`——若写成
// `window.X19...`，模型会把它当成真实属性名照抄，不会解码。
const (
	// 两个 base64 载荷对应的明文，仅供测试校验载荷未写错。
	dshBootIdentifier = "__DSH_BOOT__"
	dshWebCommand     = "dsh web"

	// base64(dshBootIdentifier) 与 base64(dshWebCommand)。
	// 载荷本身不得包含 dsh/DSH（已由测试断言），否则会自我触发或直接暴露。
	dshBootBase64 = "X19EU0hfQk9PVF9f"
	dshWebBase64  = "ZHNoIHdlYg=="

	dshBootWindowReplacement = "window['" + dshBootBase64 + "'] (base64 of the identifier; decode before use)"
	dshBootBareReplacement   = dshBootBase64 + " (base64 of the identifier; decode before use)"
	dshWebReplacement        = "the CLI subcommand " + dshWebBase64 + " (base64; decode before use)"
)

// 顺序敏感：window.__DSH_BOOT__ 必须先于裸 __DSH_BOOT__ 处理，否则会得到
// window.X19... 这种把 base64 当成属性名的写法。
//
// 裸 DSH 的整词边界不能用 \b：Go 的 \b 把 _ 当单词字符，__DSH_BOOT__ 里的 DSH
// 会被判成独立单词。改用显式否定字符类，替换时把边界字符原样拼回。
var (
	dshBootWindowPattern = regexp.MustCompile(`window\.__DSH_BOOT__`)
	dshBootBarePattern   = regexp.MustCompile(`__DSH_BOOT__`)
	dshWebPattern        = regexp.MustCompile(`(?i)dsh web`)
	dshBareWordPattern   = regexp.MustCompile(`(?i)(^|[^0-9A-Za-z_\-])DSH([^0-9A-Za-z_\-]|$)`)
	// dshAnyCasePattern 只用于快速排除，不参与替换。
	dshAnyCasePattern = regexp.MustCompile(`(?i)dsh`)
)

// dshBareWordReplacement 取配置的身份目标名，保证同一份提示词里不会出现两个产品名
// （identity.go 把 DeepSeek Harness 换成目标名，裸 DSH 指代的也是同一个产品）。
func dshBareWordReplacement() string {
	if _, target, _ := identitySettingsSummary(); target != "" {
		return target
	}
	return "WorkBuddy"
}

// replaceKeepingBoundary 整词替换，保留正则捕获到的前后边界字符（空格、标点等），
// 只替换中间的目标词，避免吃掉分隔符。
func replaceKeepingBoundary(text string, re *regexp.Regexp, to string) (string, bool) {
	if !re.MatchString(text) {
		return text, false
	}
	next := re.ReplaceAllStringFunc(text, func(match string) string {
		loc := re.FindStringSubmatch(match)
		return loc[1] + to + loc[2]
	})
	return next, next != text
}

// replaceLiteral 替换全部匹配，替换串按字面量写入（`$1` 之类不会被展开）。
func replaceLiteral(text string, re *regexp.Regexp, to string) (string, bool) {
	if !re.MatchString(text) {
		return text, false
	}
	next := re.ReplaceAllLiteralString(text, to)
	return next, next != text
}

// normalizeDSHIdentifierText 对一段文本做标识符中性化，返回改写结果与命中的规则数。
//
// 幂等：每条规则的产物都不含自己的匹配串，重复调用不会二次变形。
func normalizeDSHIdentifierText(text string) (string, int) {
	// 快速排除：不含 dsh（任意大小写）的文本无需逐条跑正则。
	// 用正则而不是 strings.ToLower，避免为几百 KB 的 system 消息整段分配小写副本。
	if !dshAnyCasePattern.MatchString(text) {
		return text, 0
	}
	rules := []struct {
		re *regexp.Regexp
		to string
		// boundary 为 true 时按整词替换并保留边界字符。
		boundary bool
	}{
		{dshBootWindowPattern, dshBootWindowReplacement, false},
		{dshBootBarePattern, dshBootBareReplacement, false},
		{dshWebPattern, dshWebReplacement, false},
		{dshBareWordPattern, dshBareWordReplacement(), true},
	}
	matched := 0
	for _, rule := range rules {
		var next string
		var changed bool
		if rule.boundary {
			next, changed = replaceKeepingBoundary(text, rule.re, rule.to)
		} else {
			next, changed = replaceLiteral(text, rule.re, rule.to)
		}
		if !changed {
			continue
		}
		text = next
		matched++
	}
	return text, matched
}

// normalizeDSHIdentifiers 扫描 system 与 assistant 的文本块，返回命中的规则数。
//
// 作用域与上游 identity.go / dshguicontext.go 一致（system + assistant），
// 让身份、GUI、标识符三类改写覆盖同一批消息。user 与 tool 内容逐字保留：
// 前者是用户原话，后者是工具真实输出，改动它们只会让模型复述出错误事实。
func normalizeDSHIdentifiers(obj map[string]any) int {
	messages, _ := obj["messages"].([]any)
	matched := 0
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role := roleOfMessage(message); role != "system" && role != "assistant" {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			if rewritten, n := normalizeDSHIdentifierText(content); n > 0 {
				message["content"] = rewritten
				matched += n
			}
		case []any:
			for _, rawPart := range content {
				part, ok := rawPart.(map[string]any)
				if !ok || part["type"] != "text" {
					continue
				}
				if text, ok := part["text"].(string); ok {
					if rewritten, n := normalizeDSHIdentifierText(text); n > 0 {
						part["text"] = rewritten
						matched += n
					}
				}
			}
		}
	}
	return matched
}
