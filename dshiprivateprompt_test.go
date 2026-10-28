package main

import (
	"net/http"
	"strings"
	"testing"
)

// 私有提示词的测试辅助：期望值直接由实现常量拼出，避免测试里再抄一份中文。

// dshTestRequest 构造一个带 DSH 客户端头的请求。
// @ 路径段本身不含任何 DSH 特征串，单段测试必须靠请求头通过 isDSHRequest 判定。
func dshTestRequest() *http.Request {
	req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("X-Client-Id", "codebuddy-cli")
	req.Header.Set("X-Ide-Name", "WorkBuddy")
	return req
}

func dshTestPrivateIdentity() string {
	enabled, name := dshPrivatePromptSettings()
	if !enabled {
		name = dshPrivateDefaultAgentName
	}
	return renderDSHPrivateIdentity("deepseek-v4.1-flash", name)
}

func dshTestPrivateGUI() string {
	return dshPrivateGUIOpening + dshPrivateGUIRest
}

// 官方 4 段原文必须能被完整识别并整段替换。
func TestDSHPrivatePromptReplacesOfficialSegments(t *testing.T) {
	setDSHPrivatePromptConfig(nil, "测试名")
	t.Cleanup(func() { setDSHPrivatePromptConfig(nil, "") })

	segments := []string{
		"You are a coding agent powered by the deepseek-v4.1-flash model.",
		"Never produce a conversation summary, checkpoint, or <analysis>/<summary> block unless the user explicitly asks for one: context compaction is handled by the harness, and a summary you volunteer replaces the work you were asked to do.",
		"Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root; absolute paths identify files or directories on the host. A trailing slash marks a directory: list it when its contents matter. Anything else is a file: use the read tool when its contents are needed, and do not claim to have inspected it before reading. @\"...\" quotes a path containing spaces.",
		"The runtime context names the DeepSeek Harness implementation checkout. The checkout location and current working directory are separate values and may differ; never infer the working directory from the checkout path. Use pwd to determine the current working directory. Use this checkout only to inspect or extend DSH itself.",
		"You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI. The browser provides no implicit DOM, route, or screenshot context. The client-plugin HMR receiver is active, but client-plugin changes reload without a refresh only while `pnpm run dev:web` is also running from this same checkout to rebuild their bundles; verify that watcher before promising automatic updates. Every other change — the apps/web shell and plain packages — requires rebuilding the affected Web artifacts and verifying this existing URL after a page refresh. Starting another server does not update this GUI. The apps/web Vite entry builds the shell but is not a standalone application because only dsh web injects window.__DSH_BOOT__. Do not start a replacement server unless the user asks; if one is needed, use a managed background job and verify its exact URL.",
	}
	for i, segment := range segments {
		obj := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": segment},
			map[string]any{"role": "user", "content": "hi"},
		}}
		if n := applyDSHPrivateSystemPrompt(obj, dshTestRequest(), 1, "deepseek-v4.1-flash"); n != 1 {
			t.Fatalf("段 %d 未被替换: n=%d", i, n)
		}
		got := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
		if strings.Contains(got, "DeepSeek Harness") || strings.Contains(got, "workspace root") ||
			strings.Contains(got, "decode before use") || strings.Contains(got, "pnpm run dev:web") {
			t.Fatalf("段 %d 替换后仍有官方特征: %q", i, got)
		}
	}
}

// 插件段与用户内容不得被改动。
func TestDSHPrivatePromptKeepsPluginSegments(t *testing.T) {
	setDSHPrivatePromptConfig(nil, "测试名")
	t.Cleanup(func() { setDSHPrivatePromptConfig(nil, "") })

	plugin := "Use the read tool — not shell commands like cat — to inspect text files. Use offset and limit to continue reading large files."
	userText := "Relative paths resolve from the workspace root; absolute paths identify files or directories on the host."
	system := "You are a coding agent powered by the deepseek-v4.1-flash model.\n\n" + plugin
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": system},
		map[string]any{"role": "user", "content": userText},
	}}
	applyDSHPrivateSystemPrompt(obj, nil, 1, "deepseek-v4.1-flash")

	messages := obj["messages"].([]any)
	got := messages[0].(map[string]any)["content"].(string)
	if !strings.Contains(got, plugin) {
		t.Fatalf("插件段被改动: %q", got)
	}
	if gotUser := messages[1].(map[string]any)["content"].(string); gotUser != userText {
		t.Fatalf("user 内容被改动: %q", gotUser)
	}
}

// 幂等：重复调用不再变化。
func TestDSHPrivatePromptIsIdempotent(t *testing.T) {
	setDSHPrivatePromptConfig(nil, "测试名")
	t.Cleanup(func() { setDSHPrivatePromptConfig(nil, "") })

	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "You are a coding agent powered by the deepseek-v4.1-flash model."},
	}}
	applyDSHPrivateSystemPrompt(obj, nil, 1, "deepseek-v4.1-flash")
	first := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	if n := applyDSHPrivateSystemPrompt(obj, dshTestRequest(), 2, "deepseek-v4.1-flash"); n != 0 {
		t.Fatalf("重复调用仍命中: n=%d", n)
	}
	if second := obj["messages"].([]any)[0].(map[string]any)["content"].(string); second != first {
		t.Fatalf("重复调用改变了内容:\nfirst=%q\nsecond=%q", first, second)
	}
}

// 客户端在官方段落后追加的内容必须保留。
func TestDSHPrivatePromptKeepsClientTail(t *testing.T) {
	setDSHPrivatePromptConfig(nil, "测试名")
	t.Cleanup(func() { setDSHPrivatePromptConfig(nil, "") })

	cases := []struct{ in, tail string }{
		{"You are an AI agent powered by DeepSeek Harness. Keep this system.", "Keep this system."},
		{"You are a coding agent powered by the deepseek-v4.1-flash model. 额外要求。", "额外要求。"},
	}
	for _, tc := range cases {
		obj := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": tc.in},
		}}
		applyDSHPrivateSystemPrompt(obj, nil, 1, "deepseek-v4.1-flash")
		got := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
		if !strings.Contains(got, tc.tail) {
			t.Fatalf("追加内容丢失: in=%q got=%q", tc.in, got)
		}
	}
}

// 模型名来自请求体，不写死。
func TestDSHPrivatePromptModelFollowsRequest(t *testing.T) {
	setDSHPrivatePromptConfig(nil, "测试名")
	t.Cleanup(func() { setDSHPrivatePromptConfig(nil, "") })

	for _, model := range []string{"deepseek-v4.1-flash", "some-other-model"} {
		obj := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": "You are a coding agent powered by the deepseek-v4.1-flash model."},
		}}
		applyDSHPrivateSystemPrompt(obj, nil, 1, model)
		got := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
		if !strings.Contains(got, model) {
			t.Fatalf("模型名未跟随请求: model=%q got=%q", model, got)
		}
	}
}

// 非 DSH 请求（如 Hermes）不得进入私有提示词分支。
func TestDSHPrivatePromptSkipsNonDSHRequests(t *testing.T) {
	setDSHPrivatePromptConfig(nil, "测试名")
	t.Cleanup(func() { setDSHPrivatePromptConfig(nil, "") })

	hermes := "你是 蓝色大肥鱼，一名编码智能体，底层模型为 hermes-flash。回答要直接。\n" +
		"    - agent-harness-selection: Use when comparing agent harnesses (dsh, Codex, OpenCode).\n" +
		"    - workbuddy-gateway-ops: Use when operating the WorkBuddy/CodeBuddy API gateway."
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": hermes},
		map[string]any{"role": "user", "content": "hi"},
	}}
	if isDSHRequest(obj, nil) {
		t.Fatal("Hermes 请求被误判为 DSH")
	}
	if n := applyDSHPrivateSystemPrompt(obj, nil, 1, "gpt-5"); n != 0 {
		t.Fatalf("Hermes 请求被改写: n=%d", n)
	}
	if got := obj["messages"].([]any)[0].(map[string]any)["content"].(string); got != hermes {
		t.Fatalf("Hermes 提示词被改动: %q", got)
	}
}

// 私有段落与官方原文不存在 >=8 token 的字面重叠（除 <analysis>/<summary> 标记名）。
func TestDSHPrivatePromptHasNoLongOverlap(t *testing.T) {
	enabled, name := dshPrivatePromptSettings()
	_ = enabled
	private := renderDSHPrivateIdentity("deepseek-v4.1-flash", name) + "\n\n" +
		dshPrivateSummaryBlock + "\n\n" + dshPrivateCheckoutBlock + "\n\n" +
		dshPrivateGUIOpening + dshPrivateGUIRest + "\n\n" + dshPrivateAtPathsBlock
	official := officialPromptSentences

	tokenize := func(s string) []string {
		var out []string
		var current strings.Builder
		flush := func() {
			if current.Len() > 0 {
				out = append(out, current.String())
				current.Reset()
			}
		}
		for _, r := range s {
			switch {
			case r == ' ' || r == '\n' || r == '\t':
				flush()
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
				r == '_', r == '/', r == '.', r == ':', r == '-', r == '@', r == '\'':
				current.WriteRune(r)
			default:
				flush()
				out = append(out, string(r))
			}
		}
		flush()
		return out
	}
	grams := func(tokens []string, n int) map[string]struct{} {
		set := make(map[string]struct{})
		for i := 0; i+n <= len(tokens); i++ {
			set[strings.Join(tokens[i:i+n], " ")] = struct{}{}
		}
		return set
	}
	for _, n := range []int{8, 10, 12, 16} {
		officialGrams := grams(tokenize(official), n)
		for gram := range grams(tokenize(private), n) {
			if _, hit := officialGrams[gram]; !hit {
				continue
			}
			// <analysis>/<summary> 是模型必须识别的标记名，唯一允许的重合。
			if strings.Contains(gram, "analysis") && strings.Contains(gram, "summary") {
				continue
			}
			t.Fatalf("私有提示词与官方存在 %d-token 重叠: %q", n, gram)
		}
	}
}

// 用户消息里贴 DSH 摘要/提示词不得让整条请求被判成 DSH。
// 回归用例：本轮 Hermes 自己的请求因为上下文压缩摘要含 "DeepSeek Harness"
// 被误判，system 被改写成了私有中文段落。
func TestDSHPrivatePromptIgnoresUserContent(t *testing.T) {
	setDSHPrivatePromptConfig(nil, "测试名")
	t.Cleanup(func() { setDSHPrivatePromptConfig(nil, "") })

	hermes := "你是 蓝色大肥鱼，一名编码智能体，底层模型为 hermes-flash。回答要直接。\n" +
		"    - agent-harness-selection: Use when comparing agent harnesses (dsh, Codex, OpenCode).\n" +
		"    - workbuddy-gateway-ops: Use when operating the WorkBuddy/CodeBuddy API gateway."
	for _, userText := range []string{
		"You are an AI agent powered by DeepSeek Harness.",
		"摘要里提到 dsh web 与 window.__DSH_BOOT__ 以及 ZHNoIHdlYg== /*b64*/。",
		"pnpm run dev:web",
		"workspace root",
	} {
		obj := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": hermes},
			map[string]any{"role": "user", "content": userText},
		}}
		if isDSHRequest(obj, nil) {
			t.Fatalf("user 内容触发了 DSH 判定: %q", userText)
		}
		if n := applyDSHPrivateSystemPrompt(obj, nil, 1, "hermes-flash"); n != 0 {
			t.Fatalf("user 内容导致 system 被改写: %q n=%d", userText, n)
		}
		if got := obj["messages"].([]any)[0].(map[string]any)["content"].(string); got != hermes {
			t.Fatalf("Hermes system 被改动: %q", got)
		}
	}
}

// @ 路径段（内置文件工具指引）必须整段替换，且不留 "workspace root"。
func TestDSHPrivatePromptReplacesAtPathsSegment(t *testing.T) {
	setDSHPrivatePromptConfig(nil, "测试名")
	t.Cleanup(func() { setDSHPrivatePromptConfig(nil, "") })

	official := "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"Relative paths resolve from the workspace root; absolute paths identify files or directories on the host. " +
		"A trailing slash marks a directory: list it when its contents matter. " +
		"Anything else is a file: use the read tool when its contents are needed, and do not claim to have inspected it before reading. " +
		"@\"...\" quotes a path containing spaces."
	// 前后各挂一条其它内置工具段，确认不误伤邻居。
	prev := "Non-zero exits are reported as `[exit code: N]` markers; investigate failures before moving on."
	next := "Use the read tool — not shell commands like cat — to inspect text files."
	system := prev + "\n\n" + official + "\n\n" + next

	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": system},
	}}
	applyDSHPrivateSystemPrompt(obj, dshTestRequest(), 1, "deepseek-v4.1-flash")
	got := obj["messages"].([]any)[0].(map[string]any)["content"].(string)

	if strings.Contains(got, "workspace root") {
		t.Fatalf("替换后仍残留 workspace root: %q", got)
	}
	if strings.Contains(got, "Tokens prefixed with @") {
		t.Fatalf("官方 @ 段未被替换: %q", got)
	}
	if !strings.Contains(got, dshPrivateAtPathsBlock) {
		t.Fatalf("私有 @ 段未写入: %q", got)
	}
	if !strings.Contains(got, prev) || !strings.Contains(got, next) {
		t.Fatalf("相邻内置工具段被改动: %q", got)
	}
	// 契约串必须保留。
	for _, keep := range []string{"@\"...\"", "read 工具"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("契约串丢失 %q: %q", keep, got)
		}
	}
}

// @ 路径段客户端追加内容必须保留，且重复调用幂等。
func TestDSHPrivatePromptAtPathsKeepsTailAndIsIdempotent(t *testing.T) {
	setDSHPrivatePromptConfig(nil, "测试名")
	t.Cleanup(func() { setDSHPrivatePromptConfig(nil, "") })

	official := "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"Relative paths resolve from the workspace root; absolute paths identify files or directories on the host. " +
		"A trailing slash marks a directory: list it when its contents matter. " +
		"Anything else is a file: use the read tool when its contents are needed, and do not claim to have inspected it before reading. " +
		"@\"...\" quotes a path containing spaces."
	tail := "额外要求：优先用 @ 引用整个目录。"
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": official + " " + tail},
	}}
	applyDSHPrivateSystemPrompt(obj, dshTestRequest(), 1, "deepseek-v4.1-flash")
	first := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(first, tail) {
		t.Fatalf("客户端追加内容丢失: %q", first)
	}
	if n := applyDSHPrivateSystemPrompt(obj, nil, 2, "deepseek-v4.1-flash"); n != 0 {
		t.Fatalf("重复调用仍命中: n=%d", n)
	}
	if second := obj["messages"].([]any)[0].(map[string]any)["content"].(string); second != first {
		t.Fatalf("重复调用改变了内容:\nfirst=%q\nsecond=%q", first, second)
	}
}

// 官方句子被截短/改措辞时，仍要识别为官方内容丢弃，不能当客户端追加内容保留。
// 回归用例：客户端把 @ 段截成 "...identify files on the host." 后，
// "workspace root" 曾被原样保留并送回上游。
func TestDSHPrivatePromptDropsTruncatedOfficialSentences(t *testing.T) {
	setDSHPrivatePromptConfig(nil, "测试名")
	t.Cleanup(func() { setDSHPrivatePromptConfig(nil, "") })

	truncated := "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"Relative paths resolve from the workspace root; absolute paths identify files on the host. " +
		"@\"...\" quotes a path containing spaces."
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": truncated},
	}}
	applyDSHPrivateSystemPrompt(obj, dshTestRequest(), 1, "deepseek-v4.1-flash")
	got := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(got, "workspace root") {
		t.Fatalf("截短的官方句被当成客户端内容保留: %q", got)
	}
	if !strings.Contains(got, dshPrivateAtPathsBlock) {
		t.Fatalf("私有 @ 段未写入: %q", got)
	}
}
