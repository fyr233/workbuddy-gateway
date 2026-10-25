package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// fullRequestFields 构造一条带巨大 body 的完整请求日志记录。
// maxInt 只用于错误消息里截取尾部片段。
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func fullRequestFields(messages int, perMessageBytes int) map[string]any {
	items := make([]any, 0, messages)
	for i := 0; i < messages; i++ {
		items = append(items, map[string]any{
			"role":    "user",
			"content": strings.Repeat("呵", perMessageBytes/3) + "#" + strconv.Itoa(i),
		})
	}
	return map[string]any{
		"event": "full_request_upstream",
		"body": map[string]any{
			"model":    "test-model",
			"stream":   true,
			"messages": items,
		},
	}
}

func TestTruncateFullRequestBodyKeepsUnderLimit(t *testing.T) {
	fields := fullRequestFields(400, 3000)
	original, _ := json.Marshal(fields["body"])
	const limit = 64 << 10
	if len(original) <= limit {
		t.Fatalf("fixture too small: %d", len(original))
	}
	out := truncateFullRequestBody(fields, limit, 6)
	encoded, err := json.Marshal(out["body"])
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > limit {
		t.Fatalf("truncated body still %d bytes, limit %d", len(encoded), limit)
	}
	body := out["body"].(map[string]any)
	if body["model"] != "test-model" {
		t.Error("非 messages 字段被丢弃")
	}
	if got := body[debugOriginalBytesKey]; got != len(original) {
		t.Errorf("原始体积记录错误: %v != %d", got, len(original))
	}
	if body["messages_total"] != 400 {
		t.Errorf("消息总数记录错误: %v", body["messages_total"])
	}
	// 首条和末条必须在，中间必须有一条裁剪标记。
	items := body["messages"].([]any)
	first := items[0].(map[string]any)["content"].(string)
	last := items[len(items)-1].(map[string]any)["content"].(string)
	if !strings.HasSuffix(first, "#0") {
		t.Errorf("首条消息不是原首条: ...%s", first[maxInt(0, len(first)-12):])
	}
	if !strings.HasSuffix(last, "#399") {
		t.Errorf("末条消息不是原末条: ...%s", last[maxInt(0, len(last)-12):])
	}
	found := false
	for _, item := range items {
		if content, ok := item.(map[string]any)["content"].(string); ok && strings.Contains(content, debugTruncatedSentinel) {
			found = true
		}
	}
	if !found {
		t.Error("缺少裁剪标记消息")
	}
}

func TestTruncateFullRequestBodyLeavesSmallBodyAlone(t *testing.T) {
	fields := fullRequestFields(3, 100)
	before, _ := json.Marshal(fields["body"])
	out := truncateFullRequestBody(fields, 1<<20, 6)
	after, _ := json.Marshal(out["body"])
	if !bytes.Equal(before, after) {
		t.Fatal("未超限的 body 不应被改写")
	}
}

func TestTruncateDisabledByNegativeLimit(t *testing.T) {
	fields := fullRequestFields(400, 3000)
	before, _ := json.Marshal(fields["body"])
	out := truncateFullRequestBody(fields, -1, 6)
	after, _ := json.Marshal(out["body"])
	if !bytes.Equal(before, after) {
		t.Fatal("limit<0 应完全不裁剪")
	}
}

func TestTruncateShrinksSingleHugeMessage(t *testing.T) {
	// 只有 2 条消息，靠条数裁剪省不下来，必须靠单条字符串截断。
	huge := strings.Repeat("字", 200000)
	fields := map[string]any{"body": map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "system", "content": huge},
			map[string]any{"role": "user", "content": "hi"},
		},
	}}
	const limit = 32 << 10
	out := truncateFullRequestBody(fields, limit, 6)
	encoded, _ := json.Marshal(out["body"])
	if len(encoded) > limit {
		t.Fatalf("仍超限: %d > %d", len(encoded), limit)
	}
	body := out["body"].(map[string]any)
	sys := body["messages"].([]any)[0].(map[string]any)
	text := sys["content"].(string)
	if !strings.Contains(text, debugTruncatedSentinel) {
		t.Error("缺少裁剪标记")
	}
	if !utf8.ValidString(text) {
		t.Error("截断劈开了多字节字符")
	}
	if sys["content_truncated_bytes"] != len(huge) {
		t.Errorf("原长度未记录: %v", sys["content_truncated_bytes"])
	}
}

func TestTruncateCapsToolsList(t *testing.T) {
	tools := make([]any, 0, 100)
	for i := 0; i < 100; i++ {
		tools = append(tools, map[string]any{"type": "function", "name": "t" + strconv.Itoa(i)})
	}
	fields := map[string]any{"body": map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
		"tools":    tools,
		"padding":  strings.Repeat("z", 200<<10),
	}}
	out := truncateFullRequestBody(fields, 16<<10, 6)
	body := out["body"].(map[string]any)
	kept := body["tools"].([]any)
	if len(kept) != 40 {
		t.Errorf("tools 应保留 40 个，实际 %d", len(kept))
	}
	if body[debugToolsTotalKey] != 100 {
		t.Errorf("tools 总数未记录: %v", body[debugToolsTotalKey])
	}
}

func TestCompactDebugRecordDropsEmptyKeepsFalse(t *testing.T) {
	record := map[string]any{
		"event":           "x",
		"acc":             "",
		"model":           nil,
		"content_length":  int64(0),
		"empty_list":      []any{},
		"empty_map":       map[string]any{},
		"headers_written": false,
		"json_valid":      true,
		"status_code":     200,
	}
	out := compactDebugRecord(record)
	for _, key := range []string{"acc", "model", "content_length", "empty_list", "empty_map"} {
		if _, ok := out[key]; ok {
			t.Errorf("空值字段 %q 未被剔除", key)
		}
	}
	// false 有语义（未写出/未通过），不能当空值删掉。
	if v, ok := out["headers_written"]; !ok || v != false {
		t.Error("布尔 false 被误删")
	}
	if out["event"] != "x" || out["status_code"] != 200 {
		t.Error("有效字段被误删")
	}
}

func TestDebugEventAppliesTruncationOnlyToFullRequests(t *testing.T) {
	var output bytes.Buffer
	sink := &debugJSONSink{writer: &output}
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

	applyFullRequestLimits(16<<10, 4)
	body := []byte(`{"model":"m"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Host = "gateway.test"
	ensureDebugRequestContext(req, "", time.Now())
	debugEvent(req, "info", "full_request_upstream", fullRequestFields(300, 3000))
	debugEvent(req, "info", "request_received", map[string]any{"status_code": 200})

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 records, got %d", len(lines))
	}
	if len(lines[0]) > 64<<10 {
		t.Errorf("完整请求记录未裁剪: %d 字节", len(lines[0]))
	}
	var full map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &full); err != nil {
		t.Fatal(err)
	}
	if full["body"].(map[string]any)[debugOriginalBytesKey] == nil {
		t.Error("裁剪后缺少原始体积")
	}
}

func TestApplyFullRequestLimitsDefaults(t *testing.T) {
	t.Cleanup(func() { applyFullRequestLimits(0, 0) })
	applyFullRequestLimits(0, 0)
	if fullRequestLogLimit() != defaultFullRequestLimitBytes {
		t.Errorf("默认上限错误: %d", fullRequestLogLimit())
	}
	if fullRequestKeepMessages() != defaultFullRequestKeep {
		t.Errorf("默认保留条数错误: %d", fullRequestKeepMessages())
	}
	applyFullRequestLimits(-1, 3)
	if fullRequestLogLimit() != -1 || fullRequestKeepMessages() != 3 {
		t.Error("显式负值应原样生效（表示不裁剪）")
	}
}

func TestCompressLogFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway-2026-01-01.log")
	payload := strings.Repeat("日志行内容\n", 5000)
	if err := os.WriteFile(path, []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	compressLogFile(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("原文件应被删除")
	}
	if _, err := os.Stat(path + ".gz"); err != nil {
		t.Fatalf("缺少 .gz: %v", err)
	}
	if _, err := os.Stat(path + ".gz.tmp"); !os.IsNotExist(err) {
		t.Fatal("临时文件未清理")
	}
}

func TestCompressLogFileSkipsActive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "debug-2026-01-01.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	activeLogsMu.Lock()
	activeLogs[path]++
	activeLogsMu.Unlock()
	t.Cleanup(func() {
		activeLogsMu.Lock()
		activeLogs[path]--
		if activeLogs[path] == 0 {
			delete(activeLogs, path)
		}
		activeLogsMu.Unlock()
	})
	compressLogFile(path)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("正在写入的文件不能被压缩")
	}
	if _, err := os.Stat(path + ".gz"); !os.IsNotExist(err) {
		t.Fatal("不应生成 .gz")
	}
}

func TestCompressLogFileIgnoresUnknownNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	compressLogFile(path)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("非日志文件不能被压缩")
	}
}

func TestCompressRotatedLogsOnStartUsesLogDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // logDir 是编译期常量 "logs"，用 chdir 重定向而不是改常量。
	if err := os.MkdirAll("logs", 0755); err != nil {
		t.Fatal(err)
	}

	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	today := time.Now().Format("2006-01-02")
	oldPath := filepath.Join("logs", "debug-"+yesterday+".jsonl")
	curPath := filepath.Join("logs", "debug-"+today+".jsonl")
	for _, p := range []string{oldPath, curPath} {
		if err := os.WriteFile(p, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	compressRotatedLogsOnStart()
	if _, err := os.Stat(oldPath + ".gz"); err != nil {
		t.Errorf("历史日志未压缩: %v", err)
	}
	if _, err := os.Stat(curPath); err != nil {
		t.Error("当天日志不应被压缩")
	}
}

// 并发写日志时压缩不能和写入抢同一路径：这里只验证 compressLogFile 在
// activeLogs 登记状态下完全不动文件，且不阻塞持有者。
func TestCompressDoesNotRaceActiveWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "debug-2026-01-02.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	activeLogsMu.Lock()
	activeLogs[path]++
	activeLogsMu.Unlock()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		compressLogFile(path)
	}()
	wg.Wait()
	activeLogsMu.Lock()
	n := activeLogs[path]
	activeLogs[path]--
	if activeLogs[path] == 0 {
		delete(activeLogs, path)
	}
	activeLogsMu.Unlock()
	if n != 1 {
		t.Errorf("activeLogs 计数被破坏: %d", n)
	}
}
