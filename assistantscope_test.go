package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAssistantScopeDefaultsToSystemOnly 守住本 fork 的核心约定：
// 默认配置下 assistant 历史逐字保留，身份 / GUI / 标识符三条链只碰 system。
// 这条用例是上一条生产事故（assistant 历史被全量品牌词替换）的回归防线。
func TestAssistantScopeDefaultsToSystemOnly(t *testing.T) {
	if assistantScopeEnabled() {
		t.Fatal("assistant 作用域默认必须是关闭的")
	}

	const official = "You are an AI agent powered by DeepSeek Harness. dsh web injects window.__DSH_BOOT__"
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": official},
		map[string]any{"role": "assistant", "content": official},
		map[string]any{"role": "user", "content": official},
	}}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	applyIdentityReplacements(obj, r, 1)
	normalizeDSHIdentifiers(obj)

	messages := obj["messages"].([]any)
	if got := messages[0].(map[string]any)["content"].(string); got == official {
		t.Error("system 应被改写，实际原样保留")
	}
	for i, role := range []string{"assistant", "user"} {
		if got := messages[i+1].(map[string]any)["content"]; got != official {
			t.Errorf("%s 内容被改写：%q", role, got)
		}
	}

	// 打开开关后 assistant 才参与改写，证明开关本身有效。
	setAssistantScope(true)
	defer setAssistantScope(false)
	obj2 := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": official},
	}}
	applyIdentityReplacements(obj2, httptest.NewRequest("POST", "/v1/chat/completions", nil), 2)
	if got := obj2["messages"].([]any)[0].(map[string]any)["content"].(string); strings.Contains(got, "DeepSeek Harness") {
		t.Error("assistantScope 打开后 assistant 应被改写")
	}
}
