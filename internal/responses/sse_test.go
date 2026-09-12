package responses

import (
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// 构造一个模拟的上游 Chat SSE 流。形态取自真实上游（delta 分片、usage 末帧）。
func chatSSE(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: ")
		b.WriteString(c)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func runTranslator(t *testing.T, sse string, toolCtx *ToolContext) ([]string, []map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	tr := NewStreamTranslator(rec, toolCtx, "test-model")
	if err := tr.Run(strings.NewReader(sse)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw := rec.Body.String()
	// 解析 event: / data: 对
	re := regexp.MustCompile(`(?m)^event: (\S+)\ndata: (.+)$`)
	var kinds []string
	var datas []map[string]any
	for _, m := range re.FindAllStringSubmatch(raw, -1) {
		kinds = append(kinds, m[1])
		var d map[string]any
		if err := json.Unmarshal([]byte(m[2]), &d); err != nil {
			t.Fatalf("事件 %s 的 data 不是合法 JSON: %v", m[1], err)
		}
		datas = append(datas, d)
	}
	return kinds, datas, rec
}

// 核心回归：output_item.done 与 response.completed 必须携带**完整文本**。
//
// Codex 不累加 response.output_text.delta（读 codex-rs/codex-api/src/sse/responses.rs
// 的 unhandled 列表可知），它只从 done/completed 取最终内容。给空串会导致
// Codex 收不到回复并反复重试——这个 bug 实测烧掉了 2.7 万 token。
func TestStreamTranslator_DoneCarriesFullText(t *testing.T) {
	sse := chatSSE(
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":", world"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"c1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`,
	)
	_, datas, _ := runTranslator(t, sse, &ToolContext{})

	const want = "Hello, world"

	// 1) output_item.done 的 message item
	var foundDone bool
	for _, d := range datas {
		if d["type"] != "response.output_item.done" {
			continue
		}
		item, _ := d["item"].(map[string]any)
		if item == nil || item["type"] != "message" {
			continue
		}
		foundDone = true
		content, _ := item["content"].([]any)
		if len(content) == 0 {
			t.Fatal("message item 无 content")
		}
		part, _ := content[0].(map[string]any)
		if got := part["text"]; got != want {
			t.Errorf("output_item.done 文本 = %q, want %q", got, want)
		}
	}
	if !foundDone {
		t.Fatal("未找到 message 的 output_item.done")
	}

	// 2) response.completed 的 output 数组
	var foundCompleted bool
	for _, d := range datas {
		if d["type"] != "response.completed" {
			continue
		}
		foundCompleted = true
		resp, _ := d["response"].(map[string]any)
		if resp == nil {
			t.Fatal("completed 无 response 对象")
		}
		// id 必填：Codex 对它做严格反序列化
		if id, _ := resp["id"].(string); id == "" {
			t.Error("completed.response.id 为空，Codex 解析会失败")
		}
		output, _ := resp["output"].([]any)
		var sawMessage bool
		for _, raw := range output {
			item, _ := raw.(map[string]any)
			if item["type"] != "message" {
				continue
			}
			sawMessage = true
			content, _ := item["content"].([]any)
			part, _ := content[0].(map[string]any)
			if got := part["text"]; got != want {
				t.Errorf("completed 里的文本 = %q, want %q", got, want)
			}
		}
		if !sawMessage {
			t.Error("completed 的 output 里缺少 message item")
		}
	}
	if !foundCompleted {
		t.Fatal("未找到 response.completed")
	}
}

// reasoning 文本同样必须在 done 里完整给出
func TestStreamTranslator_ReasoningDoneCarriesText(t *testing.T) {
	sse := chatSSE(
		`{"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"ing"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"answer"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	_, datas, _ := runTranslator(t, sse, &ToolContext{})

	var sawPart bool
	for _, d := range datas {
		if d["type"] == "response.reasoning_summary_text.done" {
			sawPart = true
			if got := d["text"]; got != "thinking" {
				t.Errorf("reasoning done 文本 = %q, want %q", got, "thinking")
			}
		}
	}
	if !sawPart {
		t.Error("缺少 reasoning_summary_text.done")
	}
}

// 事件顺序：created 必须最先，completed 必须最后
func TestStreamTranslator_EventOrder(t *testing.T) {
	sse := chatSSE(
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"x"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	kinds, _, _ := runTranslator(t, sse, &ToolContext{})
	if len(kinds) < 2 {
		t.Fatalf("事件太少: %v", kinds)
	}
	if kinds[0] != "response.created" {
		t.Errorf("首个事件应为 response.created，得到 %s", kinds[0])
	}
	if kinds[len(kinds)-1] != "response.completed" {
		t.Errorf("末尾事件应为 response.completed，得到 %s", kinds[len(kinds)-1])
	}
}

// 工具参数必须完整出现在 output_item.done 里（Codex 只从这里取）
func TestStreamTranslator_ToolArgumentsComplete(t *testing.T) {
	sse := chatSSE(
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Tokyo\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	_, datas, _ := runTranslator(t, sse, &ToolContext{})

	var sawTool bool
	for _, d := range datas {
		if d["type"] != "response.output_item.done" {
			continue
		}
		item, _ := d["item"].(map[string]any)
		if item == nil || item["type"] != "function_call" {
			continue
		}
		sawTool = true
		got := item["arguments"]
		want := `{"city":"Tokyo"}`
		if got != want {
			t.Errorf("工具参数不完整:\n got  %q\n want %q", got, want)
		}
		if item["call_id"] != "call_1" {
			t.Errorf("call_id 错误: %v", item["call_id"])
		}
	}
	if !sawTool {
		t.Error("未找到 function_call 的 output_item.done")
	}
}

// custom 工具：arguments 里的 {"content":...} 要解包成 input 原文
func TestStreamTranslator_CustomToolUnwrapped(t *testing.T) {
	patch := "*** Begin Patch\n*** End Patch"
	wrapped, _ := json.Marshal(map[string]string{"content": patch})
	// 分两片发，模拟真实流式
	half := len(wrapped) / 2
	sse := chatSSE(
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"apply_patch","arguments":`+mustJSONStr(string(wrapped[:half]))+`}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":`+mustJSONStr(string(wrapped[half:]))+`}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	ctx := &ToolContext{customNames: map[string]bool{"apply_patch": true}, chatNameToSpec: map[string]ToolSpec{}}
	_, datas, _ := runTranslator(t, sse, ctx)

	var sawCustom bool
	for _, d := range datas {
		if d["type"] != "response.output_item.done" {
			continue
		}
		item, _ := d["item"].(map[string]any)
		if item == nil || item["type"] != "custom_tool_call" {
			continue
		}
		sawCustom = true
		if got := item["input"]; got != patch {
			t.Errorf("custom 工具 input 未正确解包:\n got  %q\n want %q", got, patch)
		}
	}
	if !sawCustom {
		t.Error("未找到 custom_tool_call 的 output_item.done，apply_patch 会失败")
	}
}

// namespace 工具：扁平名要还原成 name + namespace
func TestStreamTranslator_NamespaceRestored(t *testing.T) {
	sse := chatSSE(
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"collaboration__spawn_agent","arguments":"{}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	ctx := &ToolContext{
		customNames: map[string]bool{},
		chatNameToSpec: map[string]ToolSpec{
			"collaboration__spawn_agent": {Kind: ToolNamespace, Name: "spawn_agent", Namespace: "collaboration"},
		},
	}
	_, datas, _ := runTranslator(t, sse, ctx)

	var sawNS bool
	for _, d := range datas {
		if d["type"] != "response.output_item.done" {
			continue
		}
		item, _ := d["item"].(map[string]any)
		if item == nil || item["type"] != "function_call" {
			continue
		}
		sawNS = true
		if item["name"] != "spawn_agent" {
			t.Errorf("name 未还原: %v", item["name"])
		}
		if item["namespace"] != "collaboration" {
			t.Errorf("namespace 未还原: %v", item["namespace"])
		}
	}
	if !sawNS {
		t.Error("未找到 function_call item")
	}
}

// 边界：完全空的流（无任何内容）也必须给出合法的 completed
func TestStreamTranslator_EmptyStream(t *testing.T) {
	sse := "data: [DONE]\n\n"
	kinds, datas, _ := runTranslator(t, sse, &ToolContext{})
	if len(kinds) == 0 {
		t.Fatal("空流未产出任何事件")
	}
	if kinds[len(kinds)-1] != "response.completed" {
		t.Errorf("空流应以 completed 收尾，得到 %s", kinds[len(kinds)-1])
	}
	for _, d := range datas {
		if d["type"] != "response.completed" {
			continue
		}
		resp, _ := d["response"].(map[string]any)
		if id, _ := resp["id"].(string); id == "" {
			t.Error("空流的 completed 缺少 id")
		}
	}
}

// finish_reason=length → status=incomplete（Codex 据此提示用户被截断）
func TestStreamTranslator_LengthBecomesIncomplete(t *testing.T) {
	sse := chatSSE(
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"partial"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
	)
	_, datas, _ := runTranslator(t, sse, &ToolContext{})
	for _, d := range datas {
		if d["type"] != "response.completed" {
			continue
		}
		resp, _ := d["response"].(map[string]any)
		if resp["status"] != "incomplete" {
			t.Errorf("length 应映射为 incomplete，得到 %v", resp["status"])
		}
	}
}

// usage 必须透出（Codex 用它做上下文管理）
func TestStreamTranslator_UsagePropagated(t *testing.T) {
	sse := chatSSE(
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"x"}}]}`,
		`{"id":"c1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107}}`,
	)
	_, datas, _ := runTranslator(t, sse, &ToolContext{})
	for _, d := range datas {
		if d["type"] != "response.completed" {
			continue
		}
		resp, _ := d["response"].(map[string]any)
		u, _ := resp["usage"].(map[string]any)
		if u == nil {
			t.Fatal("completed 缺 usage")
		}
		if u["output_tokens"] != float64(7) {
			t.Errorf("output_tokens = %v, want 7", u["output_tokens"])
		}
	}
}

func mustJSONStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// 非流式响应转换
func TestBuildResponse_NonStreaming(t *testing.T) {
	chat := map[string]any{
		"id":      "c1",
		"model":   "m",
		"created": float64(123),
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message": map[string]any{
					"role":              "assistant",
					"content":           "hi there",
					"reasoning_content": "let me think",
				},
			},
		},
		"usage": map[string]any{
			"prompt_tokens": float64(5), "completion_tokens": float64(2), "total_tokens": float64(7),
		},
	}
	resp := BuildResponse(chat, &ToolContext{}, "m")
	if resp.Status != "completed" {
		t.Errorf("status = %s", resp.Status)
	}
	// 顺序：reasoning → message
	if len(resp.Output) != 2 {
		t.Fatalf("output 项数 = %d, want 2", len(resp.Output))
	}
	if resp.Output[0].Type != "reasoning" {
		t.Errorf("首项应为 reasoning，得到 %s", resp.Output[0].Type)
	}
	if resp.Output[1].Type != "message" {
		t.Errorf("次项应为 message，得到 %s", resp.Output[1].Type)
	}
	if got := resp.Output[1].Content[0].Text; got != "hi there" {
		t.Errorf("正文 = %q", got)
	}
	if resp.Usage == nil || resp.Usage.OutputTokens != 2 {
		t.Errorf("usage 错误: %+v", resp.Usage)
	}
}

// 非流式：tool_calls → function_call item
func TestBuildResponse_ToolCalls(t *testing.T) {
	chat := map[string]any{
		"id": "c1", "model": "m",
		"choices": []any{
			map[string]any{
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_1",
							"type": "function",
							"function": map[string]any{
								"name": "get_weather", "arguments": `{"city":"Tokyo"}`,
							},
						},
					},
				},
			},
		},
	}
	resp := BuildResponse(chat, &ToolContext{}, "m")
	if len(resp.Output) != 1 {
		t.Fatalf("output 项数 = %d", len(resp.Output))
	}
	it := resp.Output[0]
	if it.Type != "function_call" || it.Name != "get_weather" {
		t.Errorf("item 错误: %+v", it)
	}
	if it.Arguments != `{"city":"Tokyo"}` {
		t.Errorf("arguments = %q", it.Arguments)
	}
}

// 非流式：custom 工具解包
func TestBuildResponse_CustomToolUnwrapped(t *testing.T) {
	patch := "*** Begin Patch\n*** End Patch"
	wrapped := mustJSONStr(patch)
	chat := map[string]any{
		"id": "c1", "model": "m",
		"choices": []any{
			map[string]any{
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{
						map[string]any{
							"id":   "c1",
							"type": "function",
							"function": map[string]any{
								"name":      "apply_patch",
								"arguments": `{"content":` + wrapped + `}`,
							},
						},
					},
				},
			},
		},
	}
	ctx := &ToolContext{customNames: map[string]bool{"apply_patch": true}, chatNameToSpec: map[string]ToolSpec{}}
	resp := BuildResponse(chat, ctx, "m")
	if len(resp.Output) != 1 {
		t.Fatalf("output 项数 = %d", len(resp.Output))
	}
	it := resp.Output[0]
	if it.Type != "custom_tool_call" {
		t.Errorf("custom 工具应转成 custom_tool_call，得到 %s", it.Type)
	}
	if it.Input != patch {
		t.Errorf("input 未解包:\n got  %q\n want %q", it.Input, patch)
	}
}

// 回归：Aggregate 产出的是 []map[string]any 而非 []any。
// 早先只断言 []any，导致非流式请求静默丢弃全部 tool_calls
// （症状：只返回 reasoning，Codex 拿不到工具结果而反复重试）。
func TestBuildResponse_ToolCallsAsMapSlice(t *testing.T) {
	chat := map[string]any{
		"id": "c1", "model": "m",
		"choices": []any{
			map[string]any{
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					// 关键：类型是 []map[string]any（upstream.Aggregate 的真实产物）
					"tool_calls": []map[string]any{
						{
							"id":   "call_1",
							"type": "function",
							"function": map[string]any{
								"name": "get_weather", "arguments": `{"city":"Tokyo"}`,
							},
						},
					},
				},
			},
		},
	}
	resp := BuildResponse(chat, &ToolContext{}, "m")
	if len(resp.Output) != 1 {
		t.Fatalf("output 项数 = %d, want 1（tool_calls 被丢弃了？）", len(resp.Output))
	}
	if resp.Output[0].Type != "function_call" {
		t.Errorf("type = %s, want function_call", resp.Output[0].Type)
	}
	if resp.Output[0].Name != "get_weather" {
		t.Errorf("name = %s", resp.Output[0].Name)
	}
}
