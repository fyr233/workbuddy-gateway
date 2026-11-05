package main

import "testing"

// withAssistantScope 在测试期间打开 assistant 改写作用域。
//
// 生产默认关闭：assistant 是模型已产出的对话内容，改写等于篡改会话事实。
// 但上游这批机制测试（身份 / GUI / 标识符三条链）原本把 assistant 一起覆盖
// 当作被测行为，打开开关后它们仍能完整验证改写链本身，而默认行为另行由
// TestAssistantScopeDefaultsToSystemOnly 守住。
func withAssistantScope(t *testing.T) {
	t.Helper()
	old := assistantScopeEnabled()
	setAssistantScope(true)
	t.Cleanup(func() { setAssistantScope(old) })
}
