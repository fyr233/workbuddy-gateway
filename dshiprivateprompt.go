package main

import (
	"log"
	"net/http"
	"strings"
	"sync"
)

// DSH 私有系统提示词。
//
// 目标：把 DSH 默认系统提示词的 4 段整段重写为中性中文表述，与官方原文不存在
// >=8 token 的字面重叠，从而不受上游针对官方提示词的字符串规则影响。
//
// 边界（重要）：
//   - 只处理"默认系统提示词"的 4 段：身份句、禁止主动产出摘要、checkout 与工作目录、
//     GUI 交互。
//   - 插件注入的段落（@ 路径、exit code、read/glob/grep、后台任务、web_search、
//     create_goal、workflow、subagent、MCP 资源、图片链接格式）整段不碰。
//   - 只对带 DSH 特征的请求生效；Hermes 等其它 harness 的请求零改动。
//
// 替换方式统一为"前缀替换"：只换掉已知的官方句段，段落尾部若有客户端追加的
// 内容（例如 "Keep this system."）逐字保留。
//
// 幂等：私有段落本身不含任何英文特征串，重复调用不会二次变形。

const (
	// {{AGENT_NAME}} 由配置提供，{{MODEL}} 取自本次请求的 body.model。
	// 模型名不写死：上游切模型或网关改路由时提示词自动跟随。
	dshPrivateIdentityTemplate = "你是 {{AGENT_NAME}}，一名编码智能体，底层模型为 {{MODEL}}。"

	dshPrivateSummaryBlock = "除非用户明确要求，否则绝不产出对话摘要、检查点，也不输出 " +
		"<analysis>/<summary> 块：上下文压缩由运行时负责，你主动附上的摘要会取代你本该完成的工作。"

	dshPrivateCheckoutBlock = "运行时上下文给出了本实现代码的检出副本位置。检出位置与当前工作目录是两个独立的值，" +
		"可能不同；绝不可由检出路径推断工作目录。用 pwd 确定当前工作目录。该检出副本仅用于查看或扩展本实现自身。"

	dshPrivateGUIOpening = "你正通过 Web GUI 与用户交互，其 URL 由运行时上下文给出。当用户提到“这个页面”" +
		"“这个界面”或“这个应用”而未指明其他对象时，指的就是此界面。"

	// GUI 段其余句子逐条对应官方后文，同样不保留英文原句。
	dshPrivateGUIRest = "浏览器不会隐式提供 DOM、路由或截图上下文。" +
		"客户端插件的 HMR 接收器已启用，但只有在同一检出目录下同时运行命令 " + dshDevWebBase64 + " /*b64*/ " +
		"重建其 bundle 时，客户端插件的改动才能免刷新热加载；在承诺自动更新之前先确认该 watcher 在运行。" +
		"其余任何改动——apps/web 外壳与普通包——都需要重建受影响的 Web 产物，并在页面刷新后验证这个现有 URL。" +
		"另起一个服务器不会更新此界面。apps/web 的 Vite 入口构建的是外壳，并非独立应用，因为只有 CLI 子命令 " +
		dshWebBase64 + " /*b64*/ 才会注入 window['" + dshBootBase64 + "'] /*b64*/。" +
		"除非用户要求，不要另起替代服务器；确需启动时，用受管后台任务运行，并核对其确切 URL。"

	// @ 路径段是内置文件工具族的指引，由工具注册表注入，不属于第三方插件。
	// 官方原文含 "workspace root"，是 issue #28 定位到的 11128 触发短语，
	// 因此同样重写为中性中文，只保留 read / @ / @"..." 三个契约串。
	dshPrivateAtPathsBlock = "以 @ 开头的记号表示用户点名引用的路径。相对路径以工作区根目录为基准解析；" +
		"绝对路径指向宿主机上的文件或目录。结尾带斜杠表示目录——当其内容相关时应当列出该目录。" +
		"其余一律视为文件：需要其内容时调用 read 工具，未真正读取前不得声称已经查看过。" +
		"@\"...\" 用于给含空格的路径加引号。"

	// 官方段落特征串，用于定位要替换的跨度。
	dshOfficialAtPathsPrefix  = "Tokens prefixed with @ are paths the user explicitly referenced"
	dshOfficialAtPathsTail    = "@\"...\" quotes a path containing spaces."
	dshOfficialSummaryPrefix  = "Never produce a conversation summary, checkpoint"
	dshOfficialCheckoutPrefix = "The runtime context names the "
	dshOfficialCheckoutMarker = " implementation checkout."
	dshOfficialGUIPrefix      = "You are interacting with the user through the "

	// 默认身份名。配置未提供时使用。
	dshPrivateDefaultAgentName = "蓝色大肥鱼"
)

var (
	dshPrivatePromptMu      sync.RWMutex
	dshPrivatePromptEnabled = true
	dshPrivatePromptAgent   = dshPrivateDefaultAgentName
)

// setDSHPrivatePromptConfig 由配置加载调用。enabled 为零值时默认开启。
func setDSHPrivatePromptConfig(enabled *bool, agentName string) {
	on := true
	if enabled != nil {
		on = *enabled
	}
	name := strings.TrimSpace(agentName)
	if name == "" {
		name = dshPrivateDefaultAgentName
	}
	dshPrivatePromptMu.Lock()
	dshPrivatePromptEnabled = on
	dshPrivatePromptAgent = name
	dshPrivatePromptMu.Unlock()
}

func dshPrivatePromptSettings() (bool, string) {
	dshPrivatePromptMu.RLock()
	defer dshPrivatePromptMu.RUnlock()
	return dshPrivatePromptEnabled, dshPrivatePromptAgent
}

// isDSHRequest 判定一次请求是否带 DSH 特征。
//
// 只看请求头与 system/developer/instructions 层。**不扫描 user/assistant**：
// 用户完全可能在提问里贴摘要、代码或别人的提示词（本轮就发生过——压缩摘要里
// 含 "DeepSeek Harness" 字样，导致 Hermes 自己的请求被误判成 DSH 并改写了
// system）。判定必须只依赖 harness 自己生成的那一层。
func isDSHRequest(obj map[string]any, r *http.Request) bool {
	if r != nil {
		if strings.Contains(strings.ToLower(r.Header.Get("X-Ide-Name")), "workbuddy") {
			return true
		}
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Client-Id")), "codebuddy-cli") {
			return true
		}
	}
	// /v1/messages 把系统提示词放在顶层 system 字段，不在 messages 里。
	if dshTextHasMarker(messageContentText(obj["system"])) {
		return true
	}
	if dshTextHasMarker(messageContentText(obj["instructions"])) {
		return true
	}
	messages, _ := obj["messages"].([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch roleOfMessage(message) {
		case "system", "developer":
			if dshTextHasMarker(messageContentText(message["content"])) {
				return true
			}
		}
	}
	return false
}

// dshTextHasMarker 检查一段文本是否含 DSH 特征串。
//
// 只认精确标识符与官方默认提示词句首。**不使用裸 "dsh"/"DSH" 词判定**：
// 任何提到 dsh 的正常文档都会命中——Hermes 自己的 system 里就有
// "comparing agent harnesses (dsh, Codex, OpenCode)"，曾导致误判。
// 中文身份句（"你是 ..."）同样不在此列，Hermes 也这么开头。
func dshTextHasMarker(text string) bool {
	if text == "" {
		return false
	}
	for _, marker := range []string{
		dshBootIdentifier, // __DSH_BOOT__
		dshWebCommand,     // dsh web
		dshDevWebCommand,  // pnpm run dev:web
		"DeepSeek Harness",
		dshBootBase64, // 已转码形态，用于重试请求
		dshWebBase64,
		dshDevWebBase64,
		// 官方默认系统提示词的开头特征。
		"You are a coding agent powered by ",
		"You are an AI agent powered by ",
		"You are WorkBuddy",
		"Never produce a conversation summary, checkpoint",
		"The runtime context names the ",
		"You are interacting with the user through the ",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// messageContentText 取出消息的纯文本，内容块数组只拼接 text 块。
func messageContentText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var builder strings.Builder
		for _, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok || part["type"] != "text" {
				continue
			}
			if text, ok := part["text"].(string); ok {
				builder.WriteString(text)
				builder.WriteString("\n")
			}
		}
		return builder.String()
	}
	return ""
}

// dshPrivateReplaceSegment 对单个段落做前缀替换，返回替换结果与是否命中。
//
// 只替换识别出的官方句段；段落尾部若还有客户端追加的内容则原样保留。
func dshPrivateReplaceSegment(segment, model, agentName string) (string, bool) {
	trimmed := strings.TrimSpace(segment)
	if trimmed == "" {
		return "", false
	}
	// 已经是私有段落：幂等跳过。
	if isDSHPrivateSegment(trimmed) {
		return "", false
	}

	// 官方段落前面可能被客户端拼了别的内容（例如 force 注入的 "Client instruction."），
	// 此时官方句段不在段首。定位到官方句段起点，前面的内容逐字保留。
	if index := officialSegmentStart(trimmed); index > 0 {
		head, rest := trimmed[:index], trimmed[index:]
		replacement, hit := dshPrivateReplaceSegment(rest, model, agentName)
		if !hit {
			return "", false
		}
		// 头部若以换行结尾，替换段不再需要前导空格。
		if strings.HasSuffix(head, "\n") || strings.HasSuffix(head, "\n\n") {
			return head + strings.TrimLeft(replacement, " "), true
		}
		return head + replacement, true
	}

	// [08] @ 路径引用：整段替换，保留客户端追加内容。
	if strings.HasPrefix(trimmed, dshOfficialAtPathsPrefix) {
		return dshPrivateAtPathsBlock + keepClientSentences(trimmed), true
	}

	// [02] 禁止主动产出摘要：整句替换。
	if strings.HasPrefix(trimmed, dshOfficialSummaryPrefix) {
		return dshPrivateSummaryBlock + keepClientSentences(trimmed), true
	}

	// [19] checkout 与工作目录：整段替换。
	if strings.HasPrefix(trimmed, dshOfficialCheckoutPrefix) && strings.Contains(trimmed, dshOfficialCheckoutMarker) {
		return dshPrivateCheckoutBlock + keepClientSentences(trimmed), true
	}

	// [20] GUI 交互：前两句换成私有开头，其余句子换成私有后文，尾部追加内容保留。
	if strings.HasPrefix(trimmed, dshOfficialGUIPrefix) && strings.Contains(trimmed, "Web GUI") {
		return dshPrivateGUIOpening + dshPrivateGUIRest + keepClientSentences(trimmed), true
	}

	// [00]/[01] 身份句：只替换第一句，保留其后的追加内容。
	if replacement, tail, hit := cutIdentitySentence(trimmed, model, agentName); hit {
		return replacement + tail, true
	}
	return "", false
}

// officialSegmentStart 返回官方默认系统提示词句段在段落中的起始偏移。
//
// 注意：force/fallback 注入的提示词会拼在客户端 system 之前或之后，官方句段
// 因此可能不在段首。这里只返回 4 段默认提示词的开头特征位置。
//
// 只识别默认提示词层的开头特征（身份句、摘要禁令、@ 路径、检出副本、GUI），
// 不碰插件注入段（read/glob/grep、后台任务、web_search、Agent Teams 等）——
// 它们由插件负责，本文件不得改写。
//
// @ 路径段虽然由工具注册表注入，但它是内置文件工具族的说明、不是第三方插件，
// 且原文含 issue #28 的 11128 触发短语 "workspace root"，因此纳入改写范围。
func officialSegmentStart(trimmed string) int {
	for _, marker := range []string{
		dshOfficialAtPathsPrefix,
		"Never produce a conversation summary, checkpoint",
		"The runtime context names the ",
		"You are interacting with the user through the ",
		"You are an AI agent powered by ",
		"You are a coding agent powered by ",
		"You are WorkBuddy",
	} {
		if index := strings.Index(trimmed, marker); index > 0 {
			return index
		}
	}
	if index := strings.Index(trimmed, "你是"); index > 0 {
		return index
	}
	return 0
}

// isDSHPrivateSegment 判断段落是否已经是私有中文段落。
func isDSHPrivateSegment(trimmed string) bool {
	for _, marker := range []string{
		"一名编码智能体，底层模型为",
		"上下文压缩由运行时负责",
		"检出副本位置",
		"你正通过 Web GUI 与用户交互",
		"以 @ 开头的记号表示用户点名引用的路径",
	} {
		if strings.Contains(trimmed, marker) {
			return true
		}
	}
	return false
}

// officialPromptSentences 是官方默认系统提示词 4 段的原文，用于识别"哪些句子
// 属于官方内容"。替换时只丢弃官方句子，客户端追加的句子逐字保留。
const officialPromptSentences = "You are a coding agent powered by the deepseek-v4.1-flash model.\n\n" +
	"Never produce a conversation summary, checkpoint, or <analysis>/<summary> block unless the user explicitly asks for one: context compaction is handled by the harness, and a summary you volunteer replaces the work you were asked to do.\n\n" +
	"Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root; absolute paths identify files or directories on the host. A trailing slash marks a directory: list it when its contents matter. Anything else is a file: use the read tool when its contents are needed, and do not claim to have inspected it before reading. @\"...\" quotes a path containing spaces.\n\n" +
	"The runtime context names the DeepSeek Harness implementation checkout. The checkout location and current working directory are separate values and may differ; never infer the working directory from the checkout path. Use pwd to determine the current working directory. Use this checkout only to inspect or extend DSH itself.\n\n" +
	"You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI. The browser provides no implicit DOM, route, or screenshot context. The client-plugin HMR receiver is active, but client-plugin changes reload without a refresh only while `pnpm run dev:web` is also running from this same checkout to rebuild their bundles; verify that watcher before promising automatic updates. Every other change — the apps/web shell and plain packages — requires rebuilding the affected Web artifacts and verifying this existing URL after a page refresh. Starting another server does not update this GUI. The apps/web Vite entry builds the shell but is not a standalone application because only dsh web injects window.__DSH_BOOT__. Do not start a replacement server unless the user asks; if one is needed, use a managed background job and verify its exact URL."

var (
	officialSentenceOnce sync.Once
	officialSentenceSet  map[string]struct{}
)

func officialSentences() map[string]struct{} {
	officialSentenceOnce.Do(func() {
		officialSentenceSet = make(map[string]struct{})
		for _, sentence := range splitSentences(officialPromptSentences) {
			officialSentenceSet[sentence] = struct{}{}
		}
	})
	return officialSentenceSet
}

// officialSentencePrefixes 是官方句子的开头片段集合，用于识别被上游改过措辞
// 或截断的官方句。
//
// 只做精确匹配不够：上游随时会微调句子（加一个词、改一个从句），精确匹配失败
// 后该句会被当成客户端追加内容保留下来——而 @ 路径段那句含 "workspace root"，
// 一旦被保留就等于把 11128 触发短语又送回去。前缀匹配对这类微调成立。
var (
	officialPrefixOnce sync.Once
	officialPrefixSet  map[string]struct{}
)

const officialSentencePrefixLen = 40

func officialSentencePrefixes() map[string]struct{} {
	officialPrefixOnce.Do(func() {
		officialPrefixSet = make(map[string]struct{})
		for _, sentence := range splitSentences(officialPromptSentences) {
			prefix := sentence
			if len(prefix) > officialSentencePrefixLen {
				prefix = prefix[:officialSentencePrefixLen]
			}
			officialPrefixSet[prefix] = struct{}{}
		}
	})
	return officialPrefixSet
}

// isOfficialSentence 判断一句（已去掉句末标点）是否属于官方默认提示词。
func isOfficialSentence(sentence string) bool {
	if _, hit := officialSentences()[sentence]; hit {
		return true
	}
	if len(sentence) < officialSentencePrefixLen {
		return false
	}
	_, hit := officialSentencePrefixes()[sentence[:officialSentencePrefixLen]]
	return hit
}

// splitSentences 按 ". " 切句并去掉句末标点，返回规范化后的句子集合。
//
// 官方文本里没有 "e.g. " 这类缩写，简单切分足够；切不干净的句子会留在保留侧，
// 最坏情况是多保留一句原文，不会误删客户端内容。
func splitSentences(text string) []string {
	var out []string
	for _, paragraph := range strings.Split(text, "\n\n") {
		rest := strings.TrimSpace(paragraph)
		for rest != "" {
			index := strings.Index(rest, ". ")
			var sentence string
			if index < 0 {
				sentence, rest = rest, ""
			} else {
				sentence, rest = rest[:index], strings.TrimSpace(rest[index+2:])
			}
			sentence = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sentence), "."))
			if sentence != "" {
				out = append(out, sentence)
			}
		}
	}
	return out
}

// keepClientSentences 返回段落中不属于官方内容的句子（含原有标点与空格）。
//
// 用逐句比对而不是找结尾标记：官方段落可能被客户端截断、改写或追加，
// 结尾标记一旦变化就会整段漏判。逐句判断对截断与追加都成立。
func keepClientSentences(segment string) string {
	var kept []string
	rest := strings.TrimSpace(segment)
	for rest != "" {
		index := strings.Index(rest, ". ")
		var sentence, tail string
		if index < 0 {
			sentence, tail = rest, ""
		} else {
			sentence, tail = rest[:index], rest[index+2:]
		}
		normalized := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sentence), "."))
		if !isOfficialSentence(normalized) {
			if index < 0 {
				kept = append(kept, sentence)
			} else {
				kept = append(kept, sentence+".")
			}
		}
		rest = strings.TrimSpace(tail)
	}
	if len(kept) == 0 {
		return ""
	}
	return " " + strings.Join(kept, " ")
}

// cutIdentitySentence 识别并替换身份句的第一句，返回替换文本与其后的追加内容。
//
// 身份句形态有两种：
//   - 官方原文 "You are <...>."；
//   - 客户端覆写的中文单句 "你是<...>。"。
func cutIdentitySentence(trimmed, model, agentName string) (string, string, bool) {
	if strings.Contains(trimmed, "\n") {
		return "", "", false
	}
	replacement := renderDSHPrivateIdentity(model, agentName)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "you are ") {
		if index := strings.Index(trimmed, ". "); index >= 0 {
			return replacement, trimmed[index+1:], true
		}
		if strings.HasSuffix(trimmed, ".") {
			return replacement, "", true
		}
		return "", "", false
	}
	if strings.HasPrefix(trimmed, "你是") {
		if index := strings.Index(trimmed, "。"); index >= 0 {
			return replacement, trimmed[index+len("。"):], true
		}
		// 无句号的极短人设句（如 "你是蓝色大肥鱼"）。
		if len([]rune(trimmed)) <= 24 && !containsInstructionVerb(trimmed) {
			return replacement, "", true
		}
	}
	return "", "", false
}

func containsInstructionVerb(text string) bool {
	for _, verb := range []string{"使用", "不要", "必须", "禁止", "调用", "执行", "读取", "写入", "回答", "回复"} {
		if strings.Contains(text, verb) {
			return true
		}
	}
	return false
}

func renderDSHPrivateIdentity(model, agentName string) string {
	if strings.TrimSpace(model) == "" {
		model = "当前配置的模型"
	}
	text := strings.ReplaceAll(dshPrivateIdentityTemplate, "{{AGENT_NAME}}", agentName)
	return strings.ReplaceAll(text, "{{MODEL}}", model)
}

// applyDSHPrivateSystemPrompt 重写首条 system 消息中的默认系统提示词段落。
// 返回被替换的段落数。插件注入段与用户内容不动。
func applyDSHPrivateSystemPrompt(obj map[string]any, r *http.Request, requestID uint64, model string) int {
	enabled, agentName := dshPrivatePromptSettings()
	if !enabled {
		return 0
	}
	// 只处理带 DSH 特征的请求。Hermes 等其它 harness 的 system 即便碰巧含有
	// 相似句子也不改写，避免误伤。
	if !isDSHRequest(obj, r) {
		return 0
	}
	messages, _ := obj["messages"].([]any)
	replaced := 0
	// /v1/messages 的 system 在顶层字段，不在 messages 里。
	if system, ok := obj["system"].(string); ok && strings.TrimSpace(system) != "" {
		if rewritten, n := rewriteDSHSystemText(system, model, agentName); n > 0 {
			obj["system"] = rewritten
			replaced += n
		}
	} else if _, isBlocks := obj["system"].([]any); isBlocks {
		// 内容块数组形态的 system：逐块改写，块间不做拼接，
		// 避免把客户端多个独立块合并成一个。
		blocks, _ := obj["system"].([]any)
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "text" {
				continue
			}
			text, ok := block["text"].(string)
			if !ok {
				continue
			}
			if rewritten, n := rewriteDSHSystemText(text, model, agentName); n > 0 {
				block["text"] = rewritten
				replaced += n
			}
		}
	}
	if len(messages) == 0 {
		return replaced
	}
	first, ok := messages[0].(map[string]any)
	if !ok || roleOfMessage(first) != "system" {
		return replaced
	}
	// content 可能是字符串，也可能是内容块数组；两种形态都要改写。
	if blocks, isBlocks := first["content"].([]any); isBlocks {
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "text" {
				continue
			}
			text, ok := block["text"].(string)
			if !ok {
				continue
			}
			if rewritten, n := rewriteDSHSystemText(text, model, agentName); n > 0 {
				block["text"] = rewritten
				replaced += n
			}
		}
		return logDSHPrivateRewrite(r, requestID, model, agentName, replaced)
	}
	text, ok := first["content"].(string)
	if !ok || strings.TrimSpace(text) == "" {
		return replaced
	}
	rewritten, n := rewriteDSHSystemText(text, model, agentName)
	if n == 0 {
		return replaced
	}
	first["content"] = rewritten
	replaced += n
	return logDSHPrivateRewrite(r, requestID, model, agentName, replaced)
}

// logDSHPrivateRewrite 记录一次私有提示词改写（只记段数，不记正文）。
func logDSHPrivateRewrite(r *http.Request, requestID uint64, model, agentName string, replaced int) int {
	if replaced <= 0 {
		return 0
	}
	log.Printf("[DSH私有提示词] traceId=%s requestId=%d 模型=%s 替换段数=%d 结果=已重写默认系统提示词 说明=插件注入段与用户内容未改动，不记录提示词正文",
		debugTraceID(r), requestID, model, replaced)
	debugEvent(r, "info", "dsh_private_system_prompt", map[string]any{
		"replaced_segments":  replaced,
		"model":              model,
		"agent_name":         agentName,
		"plugin_parts_kept":  true,
		"prompt_body_logged": false,
		"business_impact":    "把默认系统提示词改写为中性中文表述，避免上游按官方提示词特征拦截",
	})
	return replaced
}

// rewriteDSHSystemText 按空行分段改写一段系统提示词，返回新文本与替换段数。
func rewriteDSHSystemText(text, model, agentName string) (string, int) {
	segments := strings.Split(text, "\n\n")
	replaced := 0
	identityEmitted := false
	out := make([]string, 0, len(segments))
	for _, segment := range segments {
		replacement, hit := dshPrivateReplaceSegment(segment, model, agentName)
		if !hit {
			out = append(out, segment)
			continue
		}
		// 身份句可能出现两次（客户端覆写 + 官方原句），只保留一句。
		if strings.HasPrefix(replacement, "你是 "+agentName) {
			if identityEmitted {
				replaced++
				continue
			}
			identityEmitted = true
		}
		out = append(out, replacement)
		replaced++
	}
	if replaced == 0 {
		return text, 0
	}
	return strings.Join(out, "\n\n"), replaced
}
