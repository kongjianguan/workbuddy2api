// Package responses 把 OpenAI Responses API（/v1/responses）转换为网关内部的
// Chat Completions 请求，并把上游 Chat 响应/SSE 流转换回 Responses 形态。
//
// 存在的理由：Codex CLI 已移除 wire_api="chat"，只发 Responses 请求
// （codex-rs/model-provider-info/src/lib.rs：WireApi 只剩 Responses 变体），
// 而上游 CodeBuddy 只接受 Chat Completions。本包在网关内完成这层协议桥接，
// 使 Codex 能直连网关，无需外部转换层。
//
// 设计边界：
//   - 只做协议转换，不碰账号池/冷却/轮转（复用 internal/server 的既有链路）
//   - 请求方向：Responses → Chat（本包产出标准 Chat 请求体，交给既有改写管线）
//   - 响应方向：Chat → Responses（含 SSE 事件序列重建）
//
// 关键约束（读 codex 源码得出，改代码前先确认）：
//   - Codex 不累加 response.function_call_arguments.delta（sse/responses.rs:524-538
//     把它列在 unhandled），工具参数只从 response.output_item.done 一次性读取。
//     本包仍按标准发全套 delta，为其他客户端留余地，但 done 帧必须完整。
//   - Codex 对 response.completed 做严格反序列化，失败会中断流：id 字段必填。
//   - Codex 的 apply_patch 是 custom（freeform）工具，Chat 无对应概念，
//     需包装为 {"content": string} 的 function，回程再解包。
package responses

import (
	"encoding/json"
	"fmt"
)

// Request 是 /v1/responses 的请求体。
//
// 只声明转换需要的字段：Responses 规范里其余字段（store / include / text /
// prompt_cache_key / client_metadata / metadata / user 等）上游 CodeBuddy 无对应
// 概念，刻意不开字段以便静默丢弃，也避免日后误以为已支持。
type Request struct {
	Model             string          `json:"model"`
	Input             json.RawMessage `json:"input"`
	Instructions      string          `json:"instructions,omitempty"`
	Tools             []Tool          `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Reasoning         *Reasoning      `json:"reasoning,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	MaxOutputTokens   *int            `json:"max_output_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
}

// Reasoning 对应请求里的 reasoning 对象。Codex 发 {"effort":"high"}。
type Reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// ToolKind 是 Responses 工具的四种类别。
//
// Chat Completions 只有 function 一种，其余三类都要转换：
//   - custom：自由格式工具（Codex 的 apply_patch，带 lark 文法）→ 包装成 function
//   - namespace：工具分组（如 collaboration）→ 扁平化为 ns__name
//   - tool_search：客户端工具发现 → 透传（上游是否真处理见 tool_search 处理处注释）
type ToolKind int

const (
	ToolFunction ToolKind = iota
	ToolCustom
	ToolNamespace
	ToolSearch
)

func (k ToolKind) String() string {
	switch k {
	case ToolFunction:
		return "function"
	case ToolCustom:
		return "custom"
	case ToolNamespace:
		return "namespace"
	case ToolSearch:
		return "tool_search"
	}
	return "unknown"
}

// Tool 是 Responses 请求里的工具定义。
//
// 四类工具的字段布局不同（function 用扁平 name/parameters；namespace 带嵌套
// tools 数组；custom 带 format），故保留 Raw 供各类分别解析，同时提出公共字段。
type Tool struct {
	Kind        ToolKind
	Name        string
	Description string
	Parameters  json.RawMessage
	// Format 仅 custom 工具使用（Codex 的 apply_patch 是 {"type":"grammar","syntax":"lark",...}）
	Format json.RawMessage
	// Tools 仅 namespace 使用：嵌套的子工具定义
	Tools []Tool
	// Execution 仅 tool_search 使用
	Execution string
	// Raw 是原始 JSON，供需要原样透传/序列化的场景使用
	Raw json.RawMessage
}

// UnmarshalJSON 按 type 字段分派解析。
//
// 不能用一个平坦结构体收全部字段的原因：namespace 的嵌套 tools 元素与顶层
// Tool 同构但无 type 字段（默认 function），需要递归；custom 的 format 是
// 多态对象。分开解析比塞满 omitempty 字段更不易出错。
func (t *Tool) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("tool: %w", err)
	}

	t.Raw = append(json.RawMessage(nil), data...)

	switch probe.Type {
	case "function":
		t.Kind = ToolFunction
	case "custom":
		t.Kind = ToolCustom
	case "namespace":
		t.Kind = ToolNamespace
	case "tool_search":
		t.Kind = ToolSearch
	default:
		// 未知类型：保留 Raw，标为 Function 由上层决定是跳过还是透传。
		// 不报错的理由：Responses 规范在演进，新类型不应让整个请求失败。
		t.Kind = ToolFunction
	}

	var body struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Format      json.RawMessage `json:"format"`
		Execution   string          `json:"execution"`
		Tools       []Tool          `json:"tools"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return fmt.Errorf("tool %s: %w", probe.Type, err)
	}
	t.Name = body.Name
	t.Description = body.Description
	t.Parameters = body.Parameters
	t.Format = body.Format
	t.Execution = body.Execution
	t.Tools = body.Tools
	return nil
}
