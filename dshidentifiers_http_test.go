package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 全链路回归：三个协议入口各发一次真实 HTTP 请求，上游用假 transport 截获，
// 断言真正发往上游的 system 里没有明文标识符。不出网、不消耗账号额度。
func TestDSHIdentifiersOnAllProtocolEntrypoints(t *testing.T) {
	const systemBody = "Extend DSH itself when needed. Only dsh web injects window.__DSH_BOOT__."

	cases := []struct{ name, path, body string }{
		{
			name: "chat_completions",
			path: "/v1/chat/completions",
			body: `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":64,"messages":[` +
				`{"role":"system","content":"` + systemBody + `"},` +
				`{"role":"user","content":"hi"}]}`,
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: `{"model":"deepseek-v4.1-flash","stream":false,"max_output_tokens":64,"input":[` +
				`{"role":"developer","content":"` + systemBody + `"},` +
				`{"role":"user","content":"hi"}]}`,
		},
		{
			name: "messages",
			path: "/v1/messages",
			body: `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":64,"system":"` + systemBody + `",` +
				`"messages":[{"role":"user","content":"hi"}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatePolicies(t)
			resetStatsState(t)
			accountMu.Lock()
			accounts = []*Account{{Path: "synthetic.json", Edition: "cn", Auth: &StoredAuth{
				Edition: "cn",
				Auth:    StoredTokens{AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			}}}
			accountMu.Unlock()
			oldClient := cfg.HttpClient
			t.Cleanup(func() { cfg.HttpClient = oldClient })

			var captured map[string]any
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
				raw, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				// 出站可能被 gzip 压缩（official profile），先按需解压再解析
				payload := raw
				if decompressed, ok := maybeGunzip(t, raw, req.Header.Get("Content-Encoding")); ok {
					payload = decompressed
				}
				if err := json.Unmarshal(payload, &captured); err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: 200, Header: make(http.Header), Request: req,
					Body: io.NopCloser(strings.NewReader(conversationFixtureSSE(false))),
				}, nil
			})}

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			newAPIHandler().ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}

			system := collectSystemText(t, captured)
			if system == "" {
				t.Fatalf("上游请求里没有 system 文本: %#v", captured)
			}
			for _, banned := range []string{dshBootIdentifier, dshWebCommand} {
				if strings.Contains(system, banned) {
					t.Errorf("上游收到明文 %q\nsystem=%q", banned, system)
				}
			}
			if !strings.Contains(system, dshBootBase64) || !strings.Contains(system, dshWebBase64) {
				t.Errorf("没看到 base64 载荷，规则可能未生效\nsystem=%q", system)
			}
			// 裸 DSH 已换成产品名，正文其余部分保留
			if !strings.Contains(system, "itself") || !strings.Contains(system, "when needed") {
				t.Errorf("正文被破坏\nsystem=%q", system)
			}
		})
	}
}

// collectSystemText 从上游请求体里取出 system 角色的文本（含 content 数组形式）。
func collectSystemText(t *testing.T, body map[string]any) string {
	t.Helper()
	messages, _ := body["messages"].([]any)
	var parts []string
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || roleOfMessage(message) != "system" {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			parts = append(parts, content)
		case []any:
			for _, rawPart := range content {
				if part, ok := rawPart.(map[string]any); ok {
					if text, ok := part["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

// maybeGunzip 在 Content-Encoding 为 gzip 时解压；未压缩时原样返回。
func maybeGunzip(t *testing.T, raw []byte, encoding string) ([]byte, bool) {
	t.Helper()
	if !strings.Contains(strings.ToLower(encoding), "gzip") {
		return raw, false
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip 解压失败: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip 读取失败: %v", err)
	}
	return out, true
}
