package main

import (
	"encoding/json"
	"testing"
)

// 11129 body captured from WorkBuddy on 2026-10-29 (debug log line 17339).
const invalidToolSchemaBody = `{"code":11129,"msg":"invalid function call parameters","requestId":"effa0d6714a04905aa01befcbcbe4fef","extError":{"code":"invalid_function_parameters","message":"invalid function call parameters","param":"","type":"invalid_request_error","StatusCode":400},"displayTips":{"en":"The tool's parameter structure does not meet the current model's requirements and was rejected. Please start a new task or switch to another model.","zh":"工具的参数结构不符合当前模型要求，被服务端拒绝。"}}`

func TestIsModelRateLimitedIgnoresInvalidToolSchema(t *testing.T) {
	if isModelRateLimited(invalidToolSchemaBody) {
		t.Fatal("11129 invalid tool schema must not be classified as a model rate limit")
	}
	if !isInvalidToolSchemaError(invalidToolSchemaBody) {
		t.Fatal("11129 body should be recognised as an invalid tool schema error")
	}
}

func TestIsModelRateLimitedStillMatchesRealRateLimit(t *testing.T) {
	cases := []string{
		`{"code":6004,"msg":"当前使用人数较多，您也可以切换其他模型继续使用"}`,
		`{"code":6004,"msg":"busy"}`,
	}
	for _, body := range cases {
		if !isModelRateLimited(body) {
			t.Fatalf("expected rate limit classification for %s", body)
		}
	}
	if isModelRateLimited(`{"code":14018,"msg":"额度已用尽"}`) {
		t.Fatal("quota exhaustion must not be classified as a rate limit")
	}
}

func TestNormalizeRequestToolSchemasRepairsEmptyRequiredObject(t *testing.T) {
	raw := `{"model":"deepseek-v4.1-flash","tools":[
		{"type":"function","function":{"name":"delegate_task","parameters":{"type":"object","properties":{"goal":{"type":"string"}},"required":{}}}},
		{"type":"function","function":{"name":"skills_list","parameters":{"type":"object","properties":{},"required":{}}}},
		{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}},
		{"type":"function","parameters":{"type":"object","required":{}}}
	]}`
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if repaired := normalizeRequestToolSchemas(body); repaired != 3 {
		t.Fatalf("expected 3 repaired required fields, got %d", repaired)
	}

	// A second pass must be a no-op.
	if repaired := normalizeRequestToolSchemas(body); repaired != 0 {
		t.Fatalf("second pass should not repair anything, got %d", repaired)
	}

	tools := body["tools"].([]any)
	first := tools[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	req, ok := first["required"].([]any)
	if !ok {
		t.Fatalf("required should be an array, got %T", first["required"])
	}
	if len(req) != 0 {
		t.Fatalf("expected empty required array, got %v", req)
	}

	// Well-formed schema untouched.
	third := tools[2].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	req3, ok := third["required"].([]any)
	if !ok || len(req3) != 1 || req3[0] != "path" {
		t.Fatalf("valid required list must be preserved, got %v", third["required"])
	}
}

func TestNormalizeRequiredValueKeepsKeysOfNonEmptyObject(t *testing.T) {
	var body map[string]any
	if err := json.Unmarshal([]byte(`{"tools":[{"type":"function","function":{"name":"x","parameters":{"type":"object","required":{"b":"","a":""}}}}]}`), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if repaired := normalizeRequestToolSchemas(body); repaired != 1 {
		t.Fatalf("expected 1 repair, got %d", repaired)
	}
	params := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	req := params["required"].([]any)
	if len(req) != 2 || req[0] != "a" || req[1] != "b" {
		t.Fatalf("expected sorted keys [a b], got %v", req)
	}
}

func TestNormalizeToolSchemaLeavesNestedPropertiesAlone(t *testing.T) {
	var params map[string]any
	raw := `{"type":"object","properties":{"filter":{"type":"object","required":{}}},"required":["filter"]}`
	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	_, n := normalizeToolSchema(params)
	if n != 1 {
		t.Fatalf("expected 1 repair inside nested property, got %d", n)
	}
	nested := params["properties"].(map[string]any)["filter"].(map[string]any)
	if _, ok := nested["required"].([]any); !ok {
		t.Fatalf("nested required should be an array, got %T", nested["required"])
	}
}
