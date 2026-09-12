package responses

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// sseEvent 是发给客户端的单个 SSE 事件。
//
// Responses 的 SSE 每帧都带 event: 行（不同于 Chat 只靠 data 里的 type 字段）。
// 缺 event: 行会让 Codex 的解析器认不出事件类型而静默丢弃。
type sseEvent struct {
	Kind string
	Data any
}

// StreamTranslator 把上游的 Chat SSE 流翻译成 Responses SSE 流。
//
// 为什么需要状态机而不是逐帧映射：Chat 与 Responses 的流式语义差别很大——
//
//	Chat:        delta.content / delta.reasoning_content / delta.tool_calls[] 逐帧增量
//	Responses:   显式的事件生命周期（created → in_progress → item.added →
//	             content_part.added → *_text.delta → item.done → completed）
//
// 每类内容（思考/正文/工具）都要走完整的 added→delta→done 生命周期，
// 且各 item 有独立的 output_index。逐帧直译产出的事件序列会被 Codex 拒绝。
//
// Codex 的实际消费面（读 codex-rs/codex-api/src/sse/responses.rs 得出）：
//   - 真处理：created / output_item.added / output_item.done / output_text.delta /
//     reasoning_summary_text.delta / reasoning_summary_text.done / completed / failed
//   - 忽略（仅 trace）：content_part.* / output_text.done / function_call_arguments.* /
//     in_progress / reasoning_summary_part.done
//
// 忽略的不代表不发：发全套是标准行为，为将来接其他客户端留余地，
// 成本只是几个 fmt.Fprintf。但 output_item.done 必须完整——Codex 的工具参数
// 只从那里取，不累加 function_call_arguments.delta。
type StreamTranslator struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	toolCtx  *ToolContext
	respID   string
	model    string
	created  int64
	status   string
	sequence int

	// 各 item 的 output_index 分配。Codex 靠它关联增量与 item。
	nextIndex int

	// reasoning 状态
	reasoningIndex  int
	reasoningItemID string
	reasoningOpen   bool
	reasoningDone   bool
	// reasoningText 累积思考全文。done 事件必须给出完整内容——
	// Codex 不累加 delta（sse/responses.rs 把 delta 列在 unhandled），只读 done。
	reasoningText strings.Builder

	// message 状态
	messageIndex  int
	messageItemID string
	messageOpen   bool
	messageDone   bool
	// messageText 累积正文全文。同 reasoningText：done 必须完整。
	// 实测教训：done 里给空文本，Codex 会收不到任何回复并反复重试。
	messageText strings.Builder

	// 工具调用状态：按 Chat 的 tool_calls[].index 聚合
	// （Chat 把一次调用的 id/name/arguments 分多帧发，index 是聚合键）
	tools       map[int]*toolAccum
	toolOrder   []int
	toolsClosed bool

	usage *Usage
	// wroteAny 标记是否已开始写响应体（决定能否回退成错误状态码）
	wroteAny bool
}

// toolAccum 聚合一次工具调用的分片。
type toolAccum struct {
	id        string
	name      string
	args      strings.Builder
	index     int
	itemID    string
	emitted   bool // 是否已发过 output_item.added
	closed    bool
	namespace string
}

// NewStreamTranslator 构造流式翻译器。
func NewStreamTranslator(w http.ResponseWriter, toolCtx *ToolContext, model string) *StreamTranslator {
	fl, _ := w.(http.Flusher)
	t := &StreamTranslator{
		w:         w,
		flusher:   fl,
		toolCtx:   toolCtx,
		respID:    newID(responseIDPrefix),
		model:     model,
		created:   nowUnix(),
		status:    "in_progress",
		tools:     map[int]*toolAccum{},
		nextIndex: 0,
	}
	t.reasoningIndex = -1
	t.messageIndex = -1
	return t
}

// Run 消费上游 SSE 流并写出 Responses SSE。
//
// 返回写出的错误（仅在没有向客户端写过任何字节时才能改状态码）。
func (t *StreamTranslator) Run(r io.Reader) error {
	t.w.Header().Set("Content-Type", "text/event-stream")
	t.w.Header().Set("Cache-Control", "no-cache")
	t.w.Header().Set("Connection", "keep-alive")
	t.w.WriteHeader(http.StatusOK)

	t.emit("response.created", map[string]any{
		"type":     "response.created",
		"response": t.snapshot("in_progress", nil),
	})
	t.emit("response.in_progress", map[string]any{
		"type":     "response.in_progress",
		"response": t.snapshot("in_progress", nil),
	})

	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			t.finish("failed")
			return err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data: ") {
			payload := strings.TrimPrefix(trimmed, "data: ")
			if payload == "[DONE]" {
				break
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(payload), &chunk) == nil {
				t.handleChunk(chunk)
			}
		}
		if err == io.EOF {
			break
		}
	}

	// 流结束：补齐所有已开启但未关闭的 item，再发 completed。
	// 顺序很重要——Codex 可能在收到 item.done 前就收到 completed，
	// 那样工具调用会被丢弃。
	t.closeAllOpen()
	t.finish("completed")
	return nil
}

// handleChunk 处理单个 Chat SSE 数据帧。
func (t *StreamTranslator) handleChunk(chunk map[string]any) {
	if t.respID == "" {
		if id := strOf(chunk["id"]); id != "" {
			t.respID = id
		}
	}
	if m := strOf(chunk["model"]); m != "" {
		t.model = m
	}
	if u, ok := chunk["usage"].(map[string]any); ok {
		t.usage = &Usage{
			InputTokens:  intOf(u["prompt_tokens"]),
			OutputTokens: intOf(u["completion_tokens"]),
			TotalTokens:  intOf(u["total_tokens"]),
		}
	}

	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta != nil {
		// 思考内容 → reasoning item 生命周期
		if rc := strOf(delta["reasoning_content"]); rc != "" {
			t.openReasoning()
			t.reasoningText.WriteString(rc)
			t.emit("response.reasoning_summary_text.delta", map[string]any{
				"type":          "response.reasoning_summary_text.delta",
				"item_id":       t.reasoningItemID,
				"output_index":  t.reasoningIndex,
				"summary_index": 0,
				"delta":         rc,
			})
		}
		// 正文 → message item 生命周期
		if c := strOf(delta["content"]); c != "" {
			t.openMessage()
			t.messageText.WriteString(c)
			t.emit("response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       t.messageItemID,
				"output_index":  t.messageIndex,
				"content_index": 0,
				"delta":         c,
			})
		}
		// 工具调用分片 → 聚合
		if tcs, ok := delta["tool_calls"].([]any); ok {
			t.handleToolDeltas(tcs)
		}
	}
	// finish_reason 到达表示本轮内容结束，但不是流结束
	if fr := strOf(choice["finish_reason"]); fr != "" {
		if fr == "length" {
			t.status = "incomplete"
		}
	}
}

// handleToolDeltas 聚合工具调用的分片增量。
//
// Chat 把一个工具调用拆成多帧：首帧带 id/name，后续帧只带 arguments 片段，
// 用 index 关联。必须按 index 聚合，否则参数会被截断或多条调用会串味。
func (t *StreamTranslator) handleToolDeltas(tcs []any) {
	for _, raw := range tcs {
		tc, _ := raw.(map[string]any)
		if tc == nil {
			continue
		}
		idx := intOf(tc["index"])
		acc, ok := t.tools[idx]
		if !ok {
			acc = &toolAccum{index: idx}
			t.tools[idx] = acc
			t.toolOrder = append(t.toolOrder, idx)
		}
		if id := strOf(tc["id"]); id != "" && acc.id == "" {
			acc.id = id
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			if n := strOf(fn["name"]); n != "" && acc.name == "" {
				acc.name = n
			}
			if a := strOf(fn["arguments"]); a != "" {
				acc.args.WriteString(a)
			}
		}
		// 名字已知即开 item：Codex 需要 output_item.added 才知道有工具在跑。
		// 但 arguments 此时还没收完，要等流结束才发 done。
		if acc.name != "" && !acc.emitted {
			t.openTool(acc)
		}
	}
}

// openReasoning 开启 reasoning item 生命周期。
func (t *StreamTranslator) openReasoning() {
	if t.reasoningOpen {
		return
	}
	// 正文已开始说明思考阶段结束——理论上不该发生（上游先思考后正文），
	// 但真发生时先关思考再开，避免两个 item 交叉。
	if t.messageOpen {
		t.closeMessage()
	}
	t.reasoningOpen = true
	t.reasoningItemID = newID(itemReasoningPrefix)
	t.reasoningIndex = t.nextIndex
	t.nextIndex++

	t.emit("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": t.reasoningIndex,
		"item": map[string]any{
			"type":    "reasoning",
			"id":      t.reasoningItemID,
			"status":  "in_progress",
			"summary": []any{},
		},
	})
	t.emit("response.reasoning_summary_part.added", map[string]any{
		"type":          "response.reasoning_summary_part.added",
		"item_id":       t.reasoningItemID,
		"output_index":  t.reasoningIndex,
		"summary_index": 0,
		"part":          map[string]any{"type": "summary_text", "text": ""},
	})
}

// closeReasoning 关闭 reasoning item。
func (t *StreamTranslator) closeReasoning() {
	if !t.reasoningOpen || t.reasoningDone {
		return
	}
	t.reasoningDone = true
	full := t.reasoningText.String()
	t.emit("response.reasoning_summary_text.done", map[string]any{
		"type":          "response.reasoning_summary_text.done",
		"item_id":       t.reasoningItemID,
		"output_index":  t.reasoningIndex,
		"summary_index": 0,
		"text":          full,
	})
	item := map[string]any{
		"type":   "reasoning",
		"id":     t.reasoningItemID,
		"status": "completed",
		"summary": []any{
			map[string]any{"type": "summary_text", "text": full},
		},
	}
	t.emit("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": t.reasoningIndex,
		"item":         item,
	})
}

// openMessage 开启 message item 生命周期。
func (t *StreamTranslator) openMessage() {
	if t.messageOpen {
		return
	}
	// 正文开始 = 思考结束
	t.closeReasoning()
	t.messageOpen = true
	t.messageItemID = newID(itemMessagePrefix)
	t.messageIndex = t.nextIndex
	t.nextIndex++

	t.emit("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": t.messageIndex,
		"item": map[string]any{
			"type":    "message",
			"id":      t.messageItemID,
			"status":  "in_progress",
			"role":    "assistant",
			"content": []any{},
		},
	})
	t.emit("response.content_part.added", map[string]any{
		"type":          "response.content_part.added",
		"item_id":       t.messageItemID,
		"output_index":  t.messageIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

// closeMessage 关闭 message item。
func (t *StreamTranslator) closeMessage() {
	if !t.messageOpen || t.messageDone {
		return
	}
	t.messageDone = true
	full := t.messageText.String()
	t.emit("response.output_text.done", map[string]any{
		"type":          "response.output_text.done",
		"item_id":       t.messageItemID,
		"output_index":  t.messageIndex,
		"content_index": 0,
		"text":          full,
	})
	t.emit("response.content_part.done", map[string]any{
		"type":          "response.content_part.done",
		"item_id":       t.messageItemID,
		"output_index":  t.messageIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": full, "annotations": []any{}},
	})
	t.emit("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": t.messageIndex,
		"item": map[string]any{
			"type":   "message",
			"id":     t.messageItemID,
			"status": "completed",
			"role":   "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": full, "annotations": []any{}},
			},
		},
	})
}

// openTool 为一次工具调用开启 item 生命周期。
func (t *StreamTranslator) openTool(acc *toolAccum) {
	// 工具出现意味着正文/思考阶段结束
	t.closeReasoning()
	t.closeMessage()

	acc.emitted = true
	acc.index = t.nextIndex
	t.nextIndex++
	if acc.id == "" {
		acc.id = newID("call_")
	}
	acc.itemID = acc.id

	item := t.toolItemAdded(acc)
	t.emit("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": acc.index,
		"item":         item,
	})
}

// toolItemAdded 构造工具 item 的 added 帧内容。
func (t *StreamTranslator) toolItemAdded(acc *toolAccum) map[string]any {
	if t.toolCtx.isCustom(acc.name) {
		return map[string]any{
			"type":    "custom_tool_call",
			"id":      acc.id,
			"call_id": acc.id,
			"name":    acc.name,
			"status":  "in_progress",
			"input":   "",
		}
	}
	return map[string]any{
		"type":      "function_call",
		"id":        acc.id,
		"call_id":   acc.id,
		"name":      acc.name,
		"status":    "in_progress",
		"arguments": "",
	}
}

// closeTool 关闭一次工具调用的 item。
//
// 这是 Codex 真正取参数的地方（它不累加 delta），故 arguments/input 必须完整。
func (t *StreamTranslator) closeTool(acc *toolAccum) {
	if acc.closed {
		return
	}
	acc.closed = true
	rawArgs := acc.args.String()

	if t.toolCtx.isCustom(acc.name) {
		// custom 工具：解包 {"content":...} 回原文，并把完整内容作为 input。
		// Codex 的 apply_patch 靠这个字段拿到 patch 文本。
		input := unwrapCustomArguments(rawArgs)
		t.emit("response.custom_tool_call_input.delta", map[string]any{
			"type":         "response.custom_tool_call_input.delta",
			"item_id":      acc.itemID,
			"output_index": acc.index,
			"delta":        input,
		})
		t.emit("response.custom_tool_call_input.done", map[string]any{
			"type":         "response.custom_tool_call_input.done",
			"item_id":      acc.itemID,
			"output_index": acc.index,
			"input":        input,
		})
		t.emit("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": acc.index,
			"item": map[string]any{
				"type":    "custom_tool_call",
				"id":      acc.id,
				"call_id": acc.id,
				"name":    acc.name,
				"status":  "completed",
				"input":   input,
			},
		})
		return
	}

	// 普通 function：发参数增量与完成帧。
	// delta 分段发是为了兼容会累加的标准客户端；Codex 忽略它们，
	// 只读下面的 output_item.done。
	if rawArgs != "" {
		t.emit("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      acc.itemID,
			"output_index": acc.index,
			"delta":        rawArgs,
		})
	}
	t.emit("response.function_call_arguments.done", map[string]any{
		"type":         "response.function_call_arguments.done",
		"item_id":      acc.itemID,
		"output_index": acc.index,
		"arguments":    rawArgs,
	})

	// namespace 还原：拆回 name + namespace 供 Codex 派发
	name, namespace := acc.name, ""
	if spec, ok := t.toolCtx.lookup(acc.name); ok && spec.Kind == ToolNamespace {
		name, namespace = spec.Name, spec.Namespace
	}
	item := map[string]any{
		"type":      "function_call",
		"id":        acc.id,
		"call_id":   acc.id,
		"name":      name,
		"status":    "completed",
		"arguments": rawArgs,
	}
	if namespace != "" {
		item["namespace"] = namespace
	}
	t.emit("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": acc.index,
		"item":         item,
	})
}

// closeAllOpen 关闭所有仍开启的 item。
func (t *StreamTranslator) closeAllOpen() {
	t.closeReasoning()
	t.closeMessage()
	if !t.toolsClosed {
		t.toolsClosed = true
		// 按 index 顺序关闭，保证 output_index 递增
		for _, idx := range t.toolOrder {
			if acc := t.tools[idx]; acc != nil && acc.emitted {
				t.closeTool(acc)
			}
		}
		// 有名字但没来得及开 item 的工具（理论上不会发生，防御性处理）
		for _, idx := range t.toolOrder {
			if acc := t.tools[idx]; acc != nil && !acc.emitted && acc.name != "" {
				t.openTool(acc)
				t.closeTool(acc)
			}
		}
	}
}

// finish 发送终态事件。
func (t *StreamTranslator) finish(status string) {
	if status == "failed" {
		t.emit("response.failed", map[string]any{
			"type": "response.failed",
			"response": t.snapshot("failed", &ResponseError{
				Code:    "stream_error",
				Message: "upstream stream terminated unexpectedly",
			}),
		})
		return
	}
	if t.status == "incomplete" {
		status = "incomplete"
	}
	// completed 的 response 对象必须能被 Codex 严格反序列化：
	// id 必填，output 数组给出完整条目。
	//
	// 文本取累积器的最终值：Codex 不累加 delta，只认这里的完整内容。
	// 给空串会让它收不到回复并反复重试（实测烧掉 2.7 万 token 无输出）。
	var output []any
	if t.reasoningOpen {
		output = append(output, map[string]any{
			"type": "reasoning", "id": t.reasoningItemID, "status": "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": t.reasoningText.String()}},
		})
	}
	if t.messageOpen {
		output = append(output, map[string]any{
			"type": "message", "id": t.messageItemID, "status": "completed",
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": t.messageText.String(), "annotations": []any{}}},
		})
	}
	for _, idx := range t.toolOrder {
		acc := t.tools[idx]
		if acc == nil || !acc.emitted {
			continue
		}
		if t.toolCtx.isCustom(acc.name) {
			output = append(output, map[string]any{
				"type": "custom_tool_call", "id": acc.id, "call_id": acc.id,
				"name": acc.name, "status": "completed",
				"input": unwrapCustomArguments(acc.args.String()),
			})
			continue
		}
		name, ns := acc.name, ""
		if spec, ok := t.toolCtx.lookup(acc.name); ok && spec.Kind == ToolNamespace {
			name, ns = spec.Name, spec.Namespace
		}
		it := map[string]any{
			"type": "function_call", "id": acc.id, "call_id": acc.id,
			"name": name, "status": "completed", "arguments": acc.args.String(),
		}
		if ns != "" {
			it["namespace"] = ns
		}
		output = append(output, it)
	}
	t.emit("response.completed", map[string]any{
		"type":     "response.completed",
		"response": t.snapshot(status, nil, output...),
	})
}

// snapshot 构造 response 对象快照，用于 created / in_progress / completed 等事件。
func (t *StreamTranslator) snapshot(status string, rerr *ResponseError, output ...any) map[string]any {
	if output == nil {
		output = []any{}
	}
	m := map[string]any{
		"id":         t.respID,
		"object":     "response",
		"created_at": t.created,
		"status":     status,
		"model":      t.model,
		"output":     output,
	}
	if t.usage != nil {
		m["usage"] = map[string]any{
			"input_tokens":  t.usage.InputTokens,
			"output_tokens": t.usage.OutputTokens,
			"total_tokens":  t.usage.TotalTokens,
		}
	}
	if rerr != nil {
		m["error"] = map[string]any{"code": rerr.Code, "message": rerr.Message}
	}
	return m
}

// emit 写出一个 SSE 事件。
//
// 每帧都带 event: 行与递增的 sequence_number——Responses 规范如此，
// 且部分客户端（含 Codex 的部分路径）依赖 event 行而非 data 里的 type 字段。
func (t *StreamTranslator) emit(kind string, data map[string]any) {
	t.sequence++
	data["sequence_number"] = t.sequence
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(t.w, "event: %s\ndata: %s\n\n", kind, raw)
	if t.flusher != nil {
		t.flusher.Flush()
	}
	t.wroteAny = true
}

// WroteAny 报告是否已向客户端写过数据（错误处理时据此决定能否改状态码）。
func (t *StreamTranslator) WroteAny() bool { return t.wroteAny }

// BuildErrorResponse 构造错误响应体，形状与成功响应一致但带 error 字段。
func BuildErrorResponse(status string, code, message string) *Response {
	return &Response{
		ID:        newID(responseIDPrefix),
		Object:    "response",
		CreatedAt: nowUnix(),
		Status:    status,
		Output:    []OutputItem{},
		Error:     &ResponseError{Code: code, Message: message},
	}
}
