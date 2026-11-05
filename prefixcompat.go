package main

// blockedSystemPromptPrefixReplacement 是上游黑名单里那条官方身份句被改写成的目标串。
//
// 上游 v1.14.43 把 sanitizeprefix.go 整体并进 identity.go，常量随之消失；
// 我们的私有提示词层与相关测试仍引用它，因此在这里保留同一个值。
// 实际生效的目标串仍来自 identity.target 配置，这里只是常量别名。
const blockedSystemPromptPrefixReplacement = "You are WorkBuddy"
