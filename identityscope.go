package main

import "sync/atomic"

// assistantIdentityScope 控制身份改写是否作用到 assistant 消息。
//
// 默认关闭：assistant 是模型已经产出的对话内容，改写它等于篡改会话事实。
// 模型复述自己旧回答时会与用户看到的原文不一致，而且这种篡改不可逆
// （原文在网关侧就被覆盖，下游拿不到）。身份 / GUI / 标识符三类改写因此
// 一律限定在 system 与 developer 层。
//
// 上游 v1.14.30 起把 assistant 也纳入范围，动机是上游风控会扫描 assistant
// 历史里的官方身份句并返回 11128。若该错误重现，把 identity.assistantScope
// 设为 true 可立刻恢复旧行为（热加载，无需重启）。
var assistantIdentityScope atomic.Bool

func assistantScopeEnabled() bool { return assistantIdentityScope.Load() }

func setAssistantScope(v bool) { assistantIdentityScope.Store(v) }

// identityRoleInScope 是三类改写共用的作用域判断。
func identityRoleInScope(role string) bool {
	if role == "system" || role == "developer" {
		return true
	}
	return role == "assistant" && assistantScopeEnabled()
}
