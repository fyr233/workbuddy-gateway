package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 端到端验证：真实 debug sink → 真实 dailyLogWriter 文件 → 检查磁盘上的行体积。
// 单测里直接调 debugEvent 只能证明函数被调用，这条能证明写进文件的内容确实变小了。
func TestFullRequestLogOnDiskIsBounded(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll("logs", 0755); err != nil {
		t.Fatal(err)
	}
	w, err := newDailyLogWriter("debug", ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })

	sink := &debugJSONSink{writer: w, closer: w}
	debugSinkMu.Lock()
	oldSink := debugSink
	debugSink = sink
	debugSinkMu.Unlock()
	t.Cleanup(func() {
		debugSinkMu.Lock()
		debugSink = oldSink
		debugSinkMu.Unlock()
		applyFullRequestLimits(0, 0)
	})

	applyFullRequestLimits(256<<10, 6)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{}`)))
	ensureDebugRequestContext(req, "", time.Now())

	// 构造 3 MB 的完整请求记录：300 条 10 KB 消息。
	fields := fullRequestFields(300, 10000)
	raw, _ := json.Marshal(fields["body"])
	if len(raw) < 2<<20 {
		t.Fatalf("fixture 太小: %d", len(raw))
	}
	debugEvent(req, "info", "full_request_upstream", fields)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join("logs", "debug-"+time.Now().Format("2006-01-02")+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读日志失败: %v", err)
	}
	line := bytes.TrimSpace(bytes.Split(data, []byte("\n"))[0])
	if len(line) > 320<<10 {
		t.Fatalf("落盘记录 %d 字节，超过上限", len(line))
	}
	var record map[string]any
	if err := json.Unmarshal(line, &record); err != nil {
		t.Fatal(err)
	}
	body := record["body"].(map[string]any)
	if body[debugOriginalBytesKey].(float64) < 2<<20 {
		t.Errorf("原始体积记录不对: %v", body[debugOriginalBytesKey])
	}
	if !strings.Contains(mustJSON(t, body), debugTruncatedSentinel) {
		t.Error("落盘内容缺少裁剪标记")
	}
	// 公共字段的空值必须已经不在。
	if _, ok := record["acc"]; ok {
		t.Error("空值字段 acc 仍被写入")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
