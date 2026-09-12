package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// chatToolNameMaxLen 是 Chat Completions 工具名长度上限。
// 超出时 namespace 扁平化会做哈希截断（见 flattenNamespaceName）：
// 上游按此上限校验，超长工具名会导致整个请求 400。
const chatToolNameMaxLen = 64

// ToolContext 承载工具转换的跨 item 状态。
//
// 必须贯穿请求转换与响应转换两个阶段：请求阶段把 custom/namespace 工具
// 改写成 Chat 形态，响应阶段要把上游返回的名字还原成 Codex 期望的形态
// （custom 工具要解包成 custom_tool_call；namespace 工具要拆回 namespace+name）。
// 丢了它就还原不回去——这是此类桥接最容易漏的一环。
type ToolContext struct {
	// customNames 记录哪些工具原本是 custom 类型（如 apply_patch）。
	// 上游返回同名 function call 时，要解包 arguments.content 回 input 字段。
	customNames map[string]bool
	// chatNameToSpec 记录 Chat 工具名 → 原始 Responses 工具描述的映射，
	// 供响应阶段还原 namespace 与 custom 语义。
	chatNameToSpec map[string]ToolSpec
	// chatTools 是转换后发给上游的工具列表（含包装后的 custom、扁平化后的 namespace）
	chatTools []map[string]any
}

// ToolSpec 记录一个工具在 Responses 侧的身份，供响应阶段还原。
type ToolSpec struct {
	Kind      ToolKind
	Name      string // Responses 侧原始名（不含 namespace 前缀）
	Namespace string // 仅 namespace 子工具有值
}

// NewToolContext 转换工具列表，返回上下文与发给上游的工具定义。
//
// 四类工具的处理（Chat Completions 只有 function，其余都要改写）：
//   - function  → 直通
//   - custom    → 包装为 {"content": string} 的 function（freeform 工具在 Chat
//     无对应概念；Codex 的 apply_patch 用 lark 文法，把文法塞进 description）
//   - namespace → 展开为 ns__name 的多个 function（64 字符上限内哈希截断）
//   - tool_search → 原样透传（上游是否真处理未验证，见下方注释）
func NewToolContext(tools []Tool) (*ToolContext, []map[string]any) {
	ctx := &ToolContext{
		customNames:    map[string]bool{},
		chatNameToSpec: map[string]ToolSpec{},
	}
	for _, t := range tools {
		switch t.Kind {
		case ToolCustom:
			ctx.customNames[t.Name] = true
			ctx.chatNameToSpec[t.Name] = ToolSpec{Kind: ToolCustom, Name: t.Name}
			ctx.chatTools = append(ctx.chatTools, customToolToChatTool(t))
		case ToolNamespace:
			for _, nested := range t.Tools {
				chatName := flattenNamespaceName(t.Name, nested.Name)
				ctx.chatNameToSpec[chatName] = ToolSpec{
					Kind: ToolNamespace, Name: nested.Name, Namespace: t.Name,
				}
				ctx.chatTools = append(ctx.chatTools, functionToolToChatTool(chatName, nested))
			}
		case ToolSearch:
			// tool_search 不在 Chat Completions 规范内。这里的策略是原样透传，
			// 与 LiteLLM 的行为一致（实测上游接受并返回 200）。
			// 未验证的是：上游究竟据此改变了工具发现行为，还是静默忽略。
			// 无论哪种，降级都是良性的——模型看不到搜索结果就继续用现有工具。
			// 保留 Raw 是为了不丢失 description 里的降级工具元数据。
			var raw map[string]any
			if json.Unmarshal(t.Raw, &raw) == nil && raw != nil {
				ctx.chatTools = append(ctx.chatTools, raw)
			}
		default:
			ctx.chatNameToSpec[t.Name] = ToolSpec{Kind: ToolFunction, Name: t.Name}
			ctx.chatTools = append(ctx.chatTools, functionToolToChatTool(t.Name, t))
		}
	}
	return ctx, ctx.chatTools
}

// functionToolToChatTool 把 Responses 的扁平 function 工具转成 Chat 的嵌套形态。
//
// 差异：Responses 用 {type,name,description,parameters}（扁平）；
// Chat 用 {type,function:{name,description,parameters}}（嵌套一层）。
func functionToolToChatTool(name string, t Tool) map[string]any {
	fn := map[string]any{"name": name}
	if t.Description != "" {
		fn["description"] = t.Description
	}
	// parameters 必须存在且为 object：上游对缺失 parameters 的 function 会报错。
	if len(t.Parameters) > 0 {
		fn["parameters"] = json.RawMessage(t.Parameters)
	} else {
		fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return map[string]any{"type": "function", "function": fn}
}

// customToolToChatTool 把 custom（freeform）工具包装成 Chat function。
//
// 为什么是 {"content": string} 这个特定形状：custom 工具（Codex 的 apply_patch）
// 接受**原始文本**（patch 内容），而 Chat 的 function call 只能传 JSON 字符串。
// 用一个单字段对象包住原文是最小失真的做法。这个形状与 LiteLLM 的实现一致
// （litellm/responses/litellm_completion_transformation/custom_tools.py），
// 因此模型侧的行为可预期。回程靠 unwrapCustomArguments 解包。
//
// 原工具的 format 定义（Codex 传 lark 文法）不能丢——放进 description 让模型
// 知道该按什么语法产出内容。
func customToolToChatTool(t Tool) map[string]any {
	desc := t.Description
	// 把 format 里的文法定义附加到 description。Codex 的 apply_patch 依赖这段
	// 文法才能产出正确的 patch；丢了它模型会凭猜测写，patch 大概率格式错误。
	if len(t.Format) > 0 {
		var f struct {
			Type       string `json:"type"`
			Syntax     string `json:"syntax"`
			Definition string `json:"definition"`
		}
		if json.Unmarshal(t.Format, &f) == nil && f.Definition != "" {
			desc = strings.TrimSpace(desc)
			if desc != "" {
				desc += "\n\n"
			}
			desc += "Format:\n```" + f.Syntax + "\n" + f.Definition + "\n```"
		}
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        t.Name,
			"description": desc,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "The raw input for this tool, following the format specified in the tool description",
					},
				},
				"required": []string{"content"},
			},
		},
	}
}

// flattenNamespaceName 把 namespace 子工具合成为 Chat 工具名。
//
// 分隔符用双下划线：单下划线在真实工具名里太常见（list_mcp_resources），
// 双下划线冲突概率低得多。
//
// 超长时哈希截断而非直接截断：两个长名截断后可能撞成同名（如两个都以
// "mcp__server__very_long_prefix" 开头的工具），撞名会让上游拒绝或让模型
// 调错工具。哈希后缀保证唯一，代价是不可读——故只在必要时启用。
func flattenNamespaceName(namespace, name string) string {
	full := namespace + "__" + name
	if len(full) <= chatToolNameMaxLen {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "__" + hex.EncodeToString(sum[:])[:8]
	keep := chatToolNameMaxLen - len(suffix)
	if keep < 0 {
		keep = 0
	}
	// 按 rune 截断，避免切碎多字节字符（工具名可能含非 ASCII）
	var b strings.Builder
	for _, r := range full {
		if b.Len()+len(string(r)) > keep {
			break
		}
		b.WriteRune(r)
	}
	return b.String() + suffix
}

// unwrapCustomArguments 从 Chat function 的 arguments 里取出 custom 工具的原始文本。
//
// 桥接把 custom 工具包装成 {"content": "..."}（见 customToolToChatTool），
// 所以上游返回的 arguments 是这个形状的 JSON。Codex 期望的是 raw string。
//
// 容错：模型可能不按 schema 产出（直接给裸文本、或给别的字段名），
// 此时原样返回整个 arguments 字符串，而不是丢成空——有内容总比没有强，
// apply_patch 自己会因格式错误而失败并让模型重试。
func unwrapCustomArguments(arguments string) string {
	if strings.TrimSpace(arguments) == "" {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal([]byte(arguments), &obj) != nil {
		return arguments
	}
	if c, ok := obj["content"].(string); ok {
		return c
	}
	return arguments
}

// wrapCustomArguments 是 unwrapCustomArguments 的逆运算，用于请求方向：
// 把 Responses 的 custom_tool_call.input（原文）包成 Chat 的 arguments JSON。
func wrapCustomArguments(input string) string {
	b, err := json.Marshal(map[string]string{"content": input})
	if err != nil {
		return `{"content":""}`
	}
	return string(b)
}

// ChatMessage 是转换产出的 Chat 消息。
//
// Content 用 any 承载字符串或内容块数组（多模态消息需要数组形态）；
// 空内容为 nil，序列化时不出现该字段——上游对空 content 字符串的容忍度
// 不如字段缺失。
type ChatMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCalls  []map[string]any `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	// ReasoningContent 是 DeepSeek 系模型要求的思考回填字段。
	// 现有上游管线（upstream/thinking.go 的 backfillReasoningContent）也会处理，
	// 这里透传客户端给的 reasoning 以便它做回填。
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ChatRequest 是转换产出的 Chat Completions 请求体。
//
// 只带上游能用的字段；其余由网关既有的改写管线（internal/upstream/payload.go）
// 补全（强制 stream:true、thinking 注入、effort 降级、指纹脱敏）。
type ChatRequest struct {
	Model               string           `json:"model"`
	Messages            []ChatMessage    `json:"messages"`
	Tools               []map[string]any `json:"tools,omitempty"`
	ToolChoice          any              `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool            `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort     string           `json:"reasoning_effort,omitempty"`
	Stream              bool             `json:"stream"`
	MaxCompletionTokens *int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64         `json:"temperature,omitempty"`
	TopP                *float64         `json:"top_p,omitempty"`
}

// reasonEffortMax 是 effort 档位上限。Codex 可能发 "xhigh"/"max" 等网关上游
// 不认识的档位，映射到上限后交给既有管线的降级逻辑处理（它知道每个模型的
// 实际支持档位）。
var reasonEffortMax = map[string]string{
	"minimal": "minimal",
	"low":     "low",
	"medium":  "medium",
	"high":    "high",
	"xhigh":   "high",
	"max":     "high",
}

// BuildChatRequest 把 Responses 请求转换成 Chat Completions 请求。
//
// 这是转换的入口。转换后交给网关既有的上游链路，因此账号轮转、冷却、
// 提示词改写、thinking 注入等能力全部复用，本包不需要重复实现。
func BuildChatRequest(req *Request) (*ChatRequest, *ToolContext, error) {
	toolCtx, chatTools := NewToolContext(req.Tools)

	out := &ChatRequest{
		Model:             req.Model,
		Tools:             chatTools,
		ParallelToolCalls: req.ParallelToolCalls,
		Stream:            req.Stream,
		Temperature:       req.Temperature,
		TopP:              req.TopP,
	}
	if req.MaxOutputTokens != nil {
		out.MaxCompletionTokens = req.MaxOutputTokens
	}
	// reasoning.effort → reasoning_effort。Codex 常发 "high"。
	// 未知档位不猜：交给既有管线的按模型降级逻辑，它知道真实支持集。
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		if mapped, ok := reasonEffortMax[req.Reasoning.Effort]; ok {
			out.ReasoningEffort = mapped
		} else {
			out.ReasoningEffort = req.Reasoning.Effort
		}
	}

	// instructions 是 Responses 的顶层系统提示，对应 Chat 的首条 system 消息。
	// 注意网关的 custom 提示词模式会把 system 整条替换掉（prompt.Rewrite），
	// 那是刻意行为：客户端注入的模板句会被上游内容审核逐字误杀。
	if req.Instructions != "" {
		out.Messages = append(out.Messages, ChatMessage{
			Role: "system", Content: req.Instructions,
		})
	}

	msgs, err := buildMessages(req.Input, toolCtx)
	if err != nil {
		return nil, nil, err
	}
	out.Messages = append(out.Messages, msgs...)

	// tool_choice 归一化：Responses 允许字符串或对象，Chat 只认字符串/对象特定形态。
	// 具体归一由既有管线（normalizeToolChoice）负责，这里只做类型转换不丢信息。
	if len(req.ToolChoice) > 0 {
		var v any
		if json.Unmarshal(req.ToolChoice, &v) == nil {
			out.ToolChoice = v
		}
	}

	return out, toolCtx, nil
}

// buildMessages 把 input 数组转成 Chat 消息序列。
//
// 核心难点：**Codex 的 item 序列与 Chat message 不是一一对应的**。
// 实测的真实序列（抓包）：
//
//	function_call(exec_command)   ← 工具调用
//	message(output_text)          ← assistant 的正文旁白
//	function_call_output          ← 工具结果
//
// 朴素地逐 item 转换会产出 assistant(tool_calls) → assistant(content) → tool，
// 两个 assistant 相邻，且 tool 消息不紧随发起它的 assistant（Chat 协议要求
// tool 必须紧跟带 tool_calls 的 assistant）。上游会拒绝或让模型困惑。
//
// 因此用待定缓冲：把 function_call 攒起来，遇到下一条 assistant 消息时合并进同
// 一条（content 与 tool_calls 共存是合法的），或在 tool 结果前冲刷。这个聚合
// 逻辑是同类项目里最容易被写漏的部分——少写它，简单对话能过，一用工具就崩。
func buildMessages(input json.RawMessage, toolCtx *ToolContext) ([]ChatMessage, error) {
	if len(input) == 0 {
		return nil, nil
	}

	// input 可以是字符串（简写形式）或数组
	var asString string
	if json.Unmarshal(input, &asString) == nil {
		return []ChatMessage{{Role: "user", Content: asString}}, nil
	}

	var items []json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}

	var (
		msgs []ChatMessage
		// pendingToolCalls 缓冲尚未归属的 tool_calls。见上方函数注释。
		pendingToolCalls []map[string]any
		// pendingReasoning 缓冲 reasoning 文本，挂到下一条 assistant 消息上。
		pendingReasoning string
	)

	// flushPending 把缓冲的 tool_calls 落成一条 assistant 消息。
	// 用于「tool 结果即将出现」和「输入结束」两个时机。
	flushPending := func() {
		if len(pendingToolCalls) == 0 {
			return
		}
		msgs = append(msgs, ChatMessage{Role: "assistant", ToolCalls: pendingToolCalls})
		pendingToolCalls = nil
	}

	for _, raw := range items {
		var probe struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &probe) != nil {
			continue // 无法识别的 item 跳过而非报错：容忍客户端演进
		}

		switch probe.Type {
		case "message":
			var m struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(raw, &m) != nil {
				continue
			}
			role := normalizeRole(m.Role)
			content := contentToChat(m.Content)

			if role == "assistant" {
				// assistant 消息：把待定的 tool_calls 合并进来（content 与
				// tool_calls 共存合法），这样工具结果就能紧跟其后。
				msg := ChatMessage{Role: "assistant", Content: content}
				if len(pendingToolCalls) > 0 {
					msg.ToolCalls = pendingToolCalls
					pendingToolCalls = nil
				}
				if pendingReasoning != "" {
					msg.ReasoningContent = pendingReasoning
					pendingReasoning = ""
				}
				msgs = append(msgs, msg)
				continue
			}
			// user / system：先冲刷待定 tool_calls，避免顺序错乱
			flushPending()
			msgs = append(msgs, ChatMessage{Role: role, Content: content})

		case "function_call":
			fc, ok := parseFunctionCall(raw, toolCtx)
			if ok {
				pendingToolCalls = append(pendingToolCalls, fc)
			}

		case "custom_tool_call":
			// custom 工具调用（Codex 的 apply_patch）：input 是原文，要包成
			// {"content": ...} 才能作为 Chat arguments 发出。
			var c struct {
				CallID string `json:"call_id"`
				ID     string `json:"id"`
				Name   string `json:"name"`
				Input  string `json:"input"`
			}
			if json.Unmarshal(raw, &c) != nil || c.Name == "" {
				continue
			}
			// 从 input 反推工具类型：tools 列表里通常已声明（Codex 每轮都发全量
			// tools），但历史消息可能先于声明出现（如 tools 被裁剪的客户端），
			// 此时若不登记，回程就不知道该解包 arguments。登记是幂等的。
			if toolCtx != nil {
				toolCtx.customNames[c.Name] = true
			}
			id := firstNonEmpty(c.CallID, c.ID)
			pendingToolCalls = append(pendingToolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      c.Name,
					"arguments": wrapCustomArguments(c.Input),
				},
			})

		case "function_call_output", "custom_tool_call_output":
			// 工具结果：必须紧跟带 tool_calls 的 assistant，故先冲刷缓冲。
			flushPending()
			var o struct {
				CallID string          `json:"call_id"`
				Output json.RawMessage `json:"output"`
			}
			if json.Unmarshal(raw, &o) != nil {
				continue
			}
			msgs = append(msgs, ChatMessage{
				Role:       "tool",
				ToolCallID: o.CallID,
				Content:    outputToText(o.Output),
			})

		case "reasoning":
			// thinking 内容缓冲起来，挂到后续 assistant 消息的 reasoning_content。
			// 不单独成消息的理由：Chat 没有 reasoning 角色；而 DeepSeek 系
			// 要求带 tool_calls 的 assistant 必须带 reasoning_content，故挂载
			// 比丢弃更有利。
			pendingReasoning += reasoningText(raw)

		case "tool_search_output":
			// 工具搜索结果里可能带新工具定义。Codex 期望这些工具在后续请求的
			// tools 里出现。当前实现：忽略（模型仍可用已有工具，属良性降级）。
			// TODO(P5): 收集并加入 tools，见 cc-switch 的 collect_tool_search_output_tools
			continue
		}
	}
	flushPending()
	return msgs, nil
}

// parseFunctionCall 把 function_call item 转成 Chat 的 tool_call。
func parseFunctionCall(raw json.RawMessage, toolCtx *ToolContext) (map[string]any, bool) {
	var fc struct {
		CallID    string          `json:"call_id"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Namespace string          `json:"namespace"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(raw, &fc) != nil || fc.Name == "" {
		return nil, false
	}
	name := fc.Name
	if fc.Namespace != "" {
		name = flattenNamespaceName(fc.Namespace, fc.Name)
	}
	// arguments 是 JSON 字符串（Chat 规范），但 Codex 偶尔给对象，统一成字符串
	args := ""
	if len(fc.Arguments) > 0 {
		var s string
		if json.Unmarshal(fc.Arguments, &s) == nil {
			args = s
		} else {
			args = string(fc.Arguments)
		}
	}
	return map[string]any{
		"id":   firstNonEmpty(fc.CallID, fc.ID),
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": args,
		},
	}, true
}

// reasoningText 提取 reasoning item 的文本。
//
// 优先 summary（Codex 默认只回传摘要），退回 content。两者都是数组形态：
// summary: [{type:"summary_text", text:"..."}]，content: [{type:"reasoning_text", text:"..."}]
func reasoningText(raw json.RawMessage) string {
	var r struct {
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return ""
	}
	var b strings.Builder
	for _, s := range r.Summary {
		b.WriteString(s.Text)
	}
	if b.Len() == 0 {
		for _, c := range r.Content {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// contentToChat 把 Responses 的 content 数组转成 Chat 的 content。
//
// Responses: [{type:"input_text"|"output_text",text},{type:"input_image",image_url}]
// Chat:      字符串，或 [{type:"text",text},{type:"image_url",image_url:{url}}]
//
// 只有文本时返回字符串（多数上游对字符串形态兼容性最好）；含图片时返回数组。
func contentToChat(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var (
		texts  []string
		blocks []map[string]any
		hasImg bool
	)
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			if p.Text != "" {
				texts = append(texts, p.Text)
				blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
			}
		case "input_image", "image_url":
			if p.ImageURL != "" {
				hasImg = true
				blocks = append(blocks, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": p.ImageURL},
				})
			}
		}
	}
	if !hasImg {
		return strings.Join(texts, "\n")
	}
	return blocks
}

// outputToText 把工具输出的多种形态统一成字符串。
//
// codex 的 function_call_output.output 通常是字符串；但规范允许内容块数组
// （含图片）。Chat 的 tool 消息只收字符串，故数组形态序列化回 JSON 文本——
// 信息不丢，模型仍能读到结构化内容。
func outputToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// normalizeRole 归一化消息角色。
//
// Responses 用 "developer" 表示系统提示的一部分，Chat 只有 system。
// 上游对 role 做白名单校验，developer 会命中 400 code=11-128。
// 注：既有管线（upstream/payload.go 的 normalizeRoles）也会做这层归一，
// 这里是提前归一以免依赖管线细节。
func normalizeRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "developer":
		return "system"
	case "system", "user", "assistant", "tool":
		return strings.ToLower(strings.TrimSpace(role))
	default:
		return "user"
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
