#!/usr/bin/env python3
"""在 upstream HEAD 上重放本地补丁（第 1 步：工具 schema + 11129 误判 + 集成点）。

按 skill 的纪律：不 rebase，在上游 tag 上手工重放。
"""
import pathlib
import sys

SRC = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else "/opt/data/work/wb-gateway-fork")


def sub(path, old, new, count=1):
    p = SRC / path
    t = p.read_text(encoding="utf-8")
    got = t.count(old)
    if got == 0 and new in t:
        print(f"skip {path}")
        return
    if got != count:
        raise SystemExit(f"{path}: expected {count}, got {got}: {old[:80]!r}")
    p.write_text(t.replace(old, new), encoding="utf-8")
    print(f"patch {path}")


# ---- 1. isModelRateLimited：11129 不是限流 ----
sub(
    "main.go",
    "func isModelRateLimited(body string) bool {\n"
    "\tlow := strings.ToLower(body)\n"
    "\treturn strings.Contains(body, `\"code\":6004`) || strings.Contains(body, \"切换其他模型\") || strings.Contains(low, \"switch to another model\")\n"
    "}",
    "func isInvalidToolSchemaError(body string) bool {\n"
    "\tlow := strings.ToLower(body)\n"
    "\treturn strings.Contains(body, `\"code\":11129`) ||\n"
    "\t\tstrings.Contains(low, \"invalid_function_parameters\") ||\n"
    "\t\tstrings.Contains(low, \"invalid function call parameters\")\n"
    "}\n"
    "\n"
    "// isModelRateLimited 只认真正的模型级限流。刻意不匹配英文\n"
    "// \"switch to another model\"：WorkBuddy 把这句话放进 11129（工具 schema 非法）\n"
    "// 的 displayTips 里，把它当限流会因一次客户端参数错误冷却整个账号池 60 秒，\n"
    "// 对外表现为 503 并让整组流量切到备用账号。\n"
    "func isModelRateLimited(body string) bool {\n"
    "\tif isInvalidToolSchemaError(body) {\n"
    "\t\treturn false\n"
    "\t}\n"
    "\treturn strings.Contains(body, `\"code\":6004`) || strings.Contains(body, \"切换其他模型\")\n"
    "}",
)

# ---- 2. 三个入口统一修复畸形工具 schema ----
sub(
    "outboundcompat.go",
    "\tapplyThinkingRules(obj, model)\n\tnormalizeDeveloperRoles(obj, r, requestID)",
    "\tapplyThinkingRules(obj, model)\n"
    "\t// 修正客户端畸形工具定义（如 `required` 被序列化成 `{}`）。\n"
    "\t// WorkBuddy 以 400/11129 拒绝这类定义；网关过去会把它误判为模型限流。\n"
    "\tif repaired := normalizeRequestToolSchemas(obj); repaired > 0 {\n"
    "\t\tlog.Printf(\"[工具schema] traceId=%s requestId=%d 修正字段数=%d 结果=required 已归一化为数组\", trace, requestID, repaired)\n"
    "\t}\n"
    "\tnormalizeDeveloperRoles(obj, r, requestID)",
)

# ---- 3. Anthropic / Responses 工具转换也过一遍 ----
sub(
    "messages.go",
    "\t\tif p, ok := t[\"input_schema\"]; ok && p != nil {\n\t\t\tfn[\"parameters\"] = p",
    "\t\tif p, ok := t[\"input_schema\"]; ok && p != nil {\n"
    "\t\t\t// 畸形 schema（required 为对象/空值）会被上游以 11129 拒绝。\n"
    "\t\t\tp, _ = normalizeToolSchema(p)\n"
    "\t\t\tfn[\"parameters\"] = p",
)
print("done step1")
