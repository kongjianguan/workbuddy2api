package responses

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Response 是 /v1/responses 的响应体。
//
// 只声明 Codex 会读取的字段。Codex 对 response.completed 做**严格反序列化**
// （codex-rs/codex-api/src/sse/responses.rs:489 用 serde_json::from_value::<ResponseCompleted>），
// 失败会中断整个流，故字段名与类型必须精确。
type Response struct {
	ID        string       `json:"id"`
	Object    string       `json:"object"`
	CreatedAt int64        `json:"created_at"`
	Status    string       `json:"status"`
	Model     string       `json:"model"`
	Output    []OutputItem `json:"output"`
	Usage     *Usage       `json:"usage,omitempty"`
	// Error 仅在失败响应里出现
	Error *ResponseError `json:"error,omitempty"`
}

// OutputItem 是 output 数组的元素。
//
// 用扁平结构而非多态接口：Codex 侧对 ResponseItem 做 tag="type" 的反序列化，
// 我们只需产出它认识的三种形态（message / reasoning / function_call 或
// custom_tool_call），每种用到不同的字段子集。
type OutputItem struct {
	Type   string `json:"type"`
	ID     string `json:"id,omitempty"`
	Status string `json:"status,omitempty"`

	// message 专用
	Role    string        `json:"role,omitempty"`
	Content []ContentPart `json:"content,omitempty"`

	// reasoning 专用
	Summary          []SummaryPart `json:"summary,omitempty"`
	Content2         []ContentPart `json:"-"` // 占位避免混淆；reasoning 的内容走 Summary
	EncryptedContent *string       `json:"encrypted_content,omitempty"`

	// function_call / custom_tool_call / tool_search_call 专用
	Name string `json:"name,omitempty"`
	// Arguments 承载两种形态：function_call 是 JSON **字符串**（Chat 规范），
	// tool_search_call 是 **对象**（Codex 的 ToolSearchCall.arguments 是 Value）。
	// 用 any 而非 string 以免为后者额外开字段。
	Arguments any    `json:"arguments,omitempty"`
	Input     string `json:"input,omitempty"` // custom_tool_call：原始文本
	CallID    string `json:"call_id,omitempty"`
	Namespace string `json:"namespace,omitempty"`

	// tool_search_call 专用：Codex 靠 execution 字段识别并执行客户端搜索。
	Execution string `json:"execution,omitempty"`
}

// ContentPart 是 message 的内容块。
type ContentPart struct {
	Type        string        `json:"type"`
	Text        string        `json:"text,omitempty"`
	Annotations []interface{} `json:"annotations,omitempty"`
}

// SummaryPart 是 reasoning item 的摘要块。
type SummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Usage 是 token 统计。Codex 读 input_tokens/output_tokens 用于上下文管理。
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ResponseError 是失败响应里的错误体。
type ResponseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// responseIDPrefix 用于生成响应 id。Codex 只要求 id 存在且非空。
const responseIDPrefix = "resp_"

// itemIDPrefix / callIDPrefix 用于生成 item 级 id。
const (
	itemMessagePrefix   = "msg_"
	itemReasoningPrefix = "rs_"
)

// newID 生成带前缀的随机 id。
// 不用 UUID 库：id 只作标识，无需全局唯一性保证，时间戳+计数器足够，
// 也避免为一个 id 引入依赖。
func newID(prefix string) string {
	return fmt.Sprintf("%s%x", prefix, time.Now().UnixNano())
}

// BuildResponse 把非流式的 Chat 响应转换成 Responses 响应。
//
// Chat 与 Responses 的核心差异：
//   - Chat 把正文、思考、工具调用塞在一条 message 里；Responses 展开成 output 数组
//   - Chat 的 tool_calls[].function.arguments 是 JSON 字符串；Responses 的
//     function_call.arguments 同样是字符串，但 custom 工具要转成 input（原文）
//   - Responses 需要显式 status 字段（completed / incomplete / failed）
func BuildResponse(chatResp map[string]any, toolCtx *ToolContext, model string) *Response {
	id := strOf(chatResp["id"])
	if id == "" {
		id = newID(responseIDPrefix)
	}
	resp := &Response{
		ID:        id,
		Object:    "response",
		CreatedAt: int64Of(chatResp["created"]),
		Status:    "completed",
		Model:     firstNonEmpty(strOf(chatResp["model"]), model),
	}
	if resp.CreatedAt == 0 {
		resp.CreatedAt = time.Now().Unix()
	}

	// usage：Codex 用它做上下文管理，缺失会导致它对上下文体量估算失准
	if u, ok := chatResp["usage"].(map[string]any); ok {
		resp.Usage = &Usage{
			InputTokens:  intOf(u["prompt_tokens"]),
			OutputTokens: intOf(u["completion_tokens"]),
			TotalTokens:  intOf(u["total_tokens"]),
		}
	}

	choices, _ := chatResp["choices"].([]any)
	if len(choices) == 0 {
		// 无 choices：返回空 output 而非报错。上游偶发空响应，
		// 让 Codex 收到一个合法的空回复比让它收到协议错误更好。
		return resp
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		return resp
	}

	// finish_reason → status：length 表示被截断，Codex 据此提示用户
	switch strOf(choice["finish_reason"]) {
	case "length":
		resp.Status = "incomplete"
	case "content_filter":
		resp.Status = "incomplete"
	}

	resp.Output = append(resp.Output, messageToOutputItems(msg, toolCtx)...)
	return resp
}

// messageToOutputItems 把一条 Chat assistant 消息拆成 Responses 的 output 数组。
//
// 顺序遵循 Codex 期望：reasoning（思考）→ message（正文）→ function_call（工具）。
// 这个顺序影响 Codex 的展示与状态机推进，不可随意调换。
func messageToOutputItems(msg map[string]any, toolCtx *ToolContext) []OutputItem {
	var items []OutputItem

	// 1. 思考内容 → reasoning item
	// DeepSeek 系把思考放在 reasoning_content，Codex 读 summary
	if rc := strOf(msg["reasoning_content"]); rc != "" {
		items = append(items, OutputItem{
			Type:   "reasoning",
			ID:     newID(itemReasoningPrefix),
			Status: "completed",
			Summary: []SummaryPart{
				{Type: "summary_text", Text: rc},
			},
		})
	}

	// 2. 正文 → message item
	if text := contentTextOf(msg["content"]); text != "" {
		items = append(items, OutputItem{
			Type:   "message",
			ID:     newID(itemMessagePrefix),
			Status: "completed",
			Role:   "assistant",
			Content: []ContentPart{
				{Type: "output_text", Text: text, Annotations: []interface{}{}},
			},
		})
	}

	// 3. 工具调用 → function_call / custom_tool_call item
	//
	// 遍历用 anyOfMaps 而非 msg["tool_calls"].([]any)：上游聚合器
	// （internal/upstream/sse.go 的 Aggregate）产出的实际类型是
	// []map[string]any，直接断言 []any 会失败并**静默丢弃全部工具调用**。
	// 实测症状：非流式请求只返回 reasoning，没有 function_call，
	// Codex 因此拿不到工具结果而反复重试。
	for _, tc := range anyOfMaps(msg["tool_calls"]) {
		fn, _ := tc["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name := strOf(fn["name"])
		args := strOf(fn["arguments"])
		callID := firstNonEmpty(strOf(tc["id"]), newID("call_"))

		// tool_search 还原：Codex 期望 type:"tool_search_call"，且 arguments 是
		// **对象**而非字符串（见 codex-rs/protocol/src/models.rs 的 ToolSearchCall
		// 与 sse/responses.rs 的 parses_tool_search_call_items 测试）。
		// 它靠 call_id + execution 关联后续回传的 tool_search_output，
		// 形态错了这条链就断了。
		if name == toolSearchName {
			var argObj any
			if json.Unmarshal([]byte(args), &argObj) != nil {
				argObj = map[string]any{}
			}
			items = append(items, OutputItem{
				Type:      "tool_search_call",
				ID:        callID,
				Status:    "completed",
				CallID:    callID,
				Execution: "client",
				Arguments: argObj,
			})
			continue
		}

		// custom 工具还原：桥接时把原文包成 {"content":...}，
		// 现在解包回 custom_tool_call.input。这一步是 apply_patch 能用的关键——
		// 不解包的话 Codex 会拿到 JSON 字符串而非 patch 文本，patch 必然失败。
		if toolCtx != nil && toolCtx.isCustom(name) {
			items = append(items, OutputItem{
				Type:   "custom_tool_call",
				ID:     callID,
				Status: "completed",
				CallID: callID,
				Name:   name,
				Input:  unwrapCustomArguments(args),
			})
			continue
		}

		// namespace 工具还原：拆回 name + namespace，Codex 靠这两个字段派发
		item := OutputItem{
			Type:      "function_call",
			ID:        callID,
			Status:    "completed",
			CallID:    callID,
			Name:      name,
			Arguments: args,
		}
		if toolCtx != nil {
			if spec, ok := toolCtx.lookup(name); ok && spec.Kind == ToolNamespace {
				item.Name = spec.Name
				item.Namespace = spec.Namespace
			}
		}
		items = append(items, item)
	}

	return items
}

// contentTextOf 提取 Chat 消息的正文文本。
func contentTextOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, raw := range t {
			if p, ok := raw.(map[string]any); ok {
				if s := strOf(p["text"]); s != "" {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// isCustom 判断某个 Chat 工具名是否来自 custom（freeform）工具。
func (c *ToolContext) isCustom(name string) bool {
	if c == nil {
		return false
	}
	return c.customNames[name]
}

// lookup 查 Chat 工具名对应的 Responses 侧身份。
func (c *ToolContext) lookup(name string) (ToolSpec, bool) {
	if c == nil {
		return ToolSpec{}, false
	}
	spec, ok := c.chatNameToSpec[name]
	return spec, ok
}

// MarshalJSON 保证 OutputItem 不带出占位字段。
func (o OutputItem) MarshalJSON() ([]byte, error) {
	// 用别名避免递归
	type alias OutputItem
	return json.Marshal(alias(o))
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func int64Of(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

// nowUnix 返回当前 Unix 时间戳（秒）。抽成函数便于测试替换。
func nowUnix() int64 { return time.Now().Unix() }

// anyOfMaps 把多种可能的 JSON 数组表示统一成 []map[string]any。
//
// 存在的理由：同一份数据在不同路径下类型不同——
//   - json.Unmarshal 到 any 得到 []any（元素是 map[string]any）
//   - 代码直接构造（如 upstream.Aggregate）得到 []map[string]any
//
// 只断言其中一种会静默丢数据。这个 helper 让转换层对两种来源都健壮。
func anyOfMaps(v any) []map[string]any {
	switch t := v.(type) {
	case []map[string]any:
		return t
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}
