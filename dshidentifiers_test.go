package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 载荷自检：base64 串必须能解回真实标识符，且本身不含 dsh，否则会自我触发或直接暴露。
func TestDSHIdentifierPayloads(t *testing.T) {
	cases := []struct{ payload, want string }{
		{dshBootBase64, dshBootIdentifier},
		{dshWebBase64, dshWebCommand},
	}
	for _, tc := range cases {
		decoded, err := base64.StdEncoding.DecodeString(tc.payload)
		if err != nil {
			t.Fatalf("载荷 %q 不是合法 base64: %v", tc.payload, err)
		}
		if string(decoded) != tc.want {
			t.Fatalf("载荷 %q 解出 %q，期望 %q", tc.payload, decoded, tc.want)
		}
		if strings.Contains(strings.ToLower(tc.payload), "dsh") {
			t.Fatalf("载荷 %q 含 dsh，会自我触发", tc.payload)
		}
	}
}

func TestNormalizeDSHIdentifierText(t *testing.T) {
	// 裸 DSH 的替换词取自 identity 配置的目标名，测试里固定成默认值。
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	setIdentityConfig(runtimeIdentityConfig{Target: "WorkBuddy"})

	cases := []struct{ name, in, want string }{
		{
			name: "window_boot",
			in:   "only dsh web injects window.__DSH_BOOT__.",
			want: "only ZHNoIHdlYg== /*b64*/ injects window['X19EU0hfQk9PVF9f'] /*b64*/.",
		},
		{
			name: "bare_boot",
			in:   "read __DSH_BOOT__ before rendering",
			want: "read X19EU0hfQk9PVF9f /*b64*/ before rendering",
		},
		{
			name: "dsh_web_uppercase",
			in:   "run DSH WEB to start",
			want: "run ZHNoIHdlYg== /*b64*/ to start",
		},
		{
			name: "dev_web_script",
			in:   "only while `pnpm run dev:web` also runs",
			want: "only while `cG5wbSBydW4gZGV2OndlYg== /*b64*/` also runs",
		},
		{
			name: "bare_dsh_word",
			in:   "you can extend DSH itself",
			want: "you can extend WorkBuddy itself",
		},
		{
			name: "bare_dsh_keeps_punctuation",
			in:   "extend (DSH), then test",
			want: "extend (WorkBuddy), then test",
		},
		{
			name: "spreadsheets_untouched",
			in:   "edit the spreadsheets",
			want: "edit the spreadsheets",
		},
		{
			name: "no_dsh_at_all",
			in:   "plain text with no identifiers",
			want: "plain text with no identifiers",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := normalizeDSHIdentifierText(tc.in)
			if got != tc.want {
				t.Fatalf("got=%q\nwant=%q", got, tc.want)
			}
		})
	}
}

// window 前缀必须保留为下标写法，不得产生 window.window。
func TestNormalizeDSHIdentifierNoDoubleWindow(t *testing.T) {
	got, _ := normalizeDSHIdentifierText("window.__DSH_BOOT__")
	if strings.Contains(got, "window.window") {
		t.Fatalf("出现 window.window: %q", got)
	}
	if !strings.HasPrefix(got, "window['") {
		t.Fatalf("window 前缀应保留为下标写法: %q", got)
	}
}

// 幂等：重复处理不二次变形，第二轮不再命中。
func TestNormalizeDSHIdentifierIdempotent(t *testing.T) {
	src := "only dsh web injects window.__DSH_BOOT__; extend DSH itself"
	once, n1 := normalizeDSHIdentifierText(src)
	if n1 == 0 {
		t.Fatal("首轮未命中")
	}
	twice, n2 := normalizeDSHIdentifierText(once)
	if n2 != 0 || twice != once {
		t.Fatalf("非幂等: n2=%d\nonce=%q\ntwice=%q", n2, once, twice)
	}
}

// 只改 system/assistant，user/tool 一律不动。
func TestNormalizeDSHIdentifiersScope(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "dsh web injects window.__DSH_BOOT__"},
		map[string]any{"role": "assistant", "content": "you can extend DSH"},
		map[string]any{"role": "user", "content": "dsh web injects window.__DSH_BOOT__"},
		map[string]any{"role": "tool", "content": "you can extend DSH"},
	}}
	if changed := normalizeDSHIdentifiers(obj); changed == 0 {
		t.Fatal("未命中任何规则")
	}
	msgs := obj["messages"].([]any)
	if strings.Contains(msgs[0].(map[string]any)["content"].(string), "__DSH_BOOT__") {
		t.Error("system 未处理")
	}
	if strings.Contains(msgs[1].(map[string]any)["content"].(string), "DSH") {
		t.Error("assistant 未处理")
	}
	if msgs[2].(map[string]any)["content"] != "dsh web injects window.__DSH_BOOT__" {
		t.Error("user 被改动")
	}
	if msgs[3].(map[string]any)["content"] != "you can extend DSH" {
		t.Error("tool 被改动")
	}
}

// 内容块数组：只处理 text 块，图片块原位不动，无关 text 块逐字保留。
func TestNormalizeDSHIdentifiersContentBlocks(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/x.png"}},
			map[string]any{"type": "text", "text": "window.__DSH_BOOT__"},
			map[string]any{"type": "text", "text": "nothing to change here"},
		}},
	}}
	normalizeDSHIdentifiers(obj)
	blocks := obj["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "image_url" {
		t.Fatal("图片块被破坏")
	}
	if blocks[1].(map[string]any)["text"] == "window.__DSH_BOOT__" {
		t.Fatal("text 块未处理")
	}
	if blocks[2].(map[string]any)["text"] != "nothing to change here" {
		t.Fatal("无关 text 块被改动")
	}
}

// 真实 DSH system 全文回归。fixture 含真实 agent 配置，不入库：缺失时跳过。
// 本地放 testdata/dsh_system.txt 即可运行。
func TestRealDSHSystemPromptIdentifiers(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("无法定位源码路径")
	}
	// 上游 TestMain 会把 cwd 切到临时目录，必须按源码位置定位 fixture。
	path := filepath.Join(filepath.Dir(file), "testdata", "dsh_system.txt")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("本地 fixture 缺失（不入库，属正常）: %v", err)
	}
	src := string(raw)
	got, n := normalizeDSHIdentifierText(src)
	if n == 0 {
		t.Fatal("真实 DSH system 未命中任何规则")
	}
	for _, banned := range []string{"__DSH_BOOT__", "dsh web"} {
		if strings.Contains(got, banned) {
			t.Errorf("残留 %q", banned)
		}
	}
	// spreadsheets 里的 dsh 子串必须保留，否则会破坏英文单词。
	if strings.Contains(src, "spreadsheets") && !strings.Contains(got, "spreadsheets") {
		t.Error("spreadsheets 被误伤")
	}
	if !strings.Contains(got, dshBootBase64) {
		t.Error("未出现 __DSH_BOOT__ 的 base64 载荷")
	}
	// 只允许规则命中的增量，防止整段文本被意外放大。
	if len(got) > len(src)+4000 {
		t.Errorf("文本异常膨胀: %d -> %d", len(src), len(got))
	}
}
