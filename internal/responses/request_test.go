package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

// 测试用的 Codex 真实请求片段（取自抓包，见 memory/codex_workbuddy2api_bridge.md）。
// 用真实形态而非构造的简化形态，因为转换的难点恰在真实数据的边界上。

func TestBuildChatRequest_TextOnly(t *testing.T) {
	req := &Request{
		Model: "deepseek-flash",
		Input: json.RawMessage(`[{"type":"message","role":"user",
			"content":[{"type":"input_text","text":"hello"}]}]`),
		Instructions: "You are Codex.",
	}
	out, _, err := BuildChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("want 2 messages (system+user), got %d: %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[0].Role != "system" || out.Messages[0].Content != "You are Codex." {
		t.Errorf("instructions 未转成 system: %+v", out.Messages[0])
	}
	if out.Messages[1].Role != "user" || out.Messages[1].Content != "hello" {
		t.Errorf("user 消息错误: %+v", out.Messages[1])
	}
}

// 核心难点回归：Codex 的 item 序列与 Chat message 非一一对应。
// 抓包得到的真实序列是 function_call → message → function_call_output，
// 朴素逐项转换会产出两个相邻 assistant 且 tool 不紧随 tool_calls，上游会拒。
func TestBuildChatRequest_ToolCallMergedWithAssistantMessage(t *testing.T) {
	req := &Request{
		Model: "deepseek-flash",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},
			{"type":"function_call","call_id":"call_1","name":"exec_command",
			 "arguments":"{\"cmd\":\"ls\"}"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Let me check."}]},
			{"type":"function_call_output","call_id":"call_1","output":"file.txt"}
		]`),
	}
	out, _, err := BuildChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	// 期望：user, assistant(content+tool_calls 合并), tool
	if len(out.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d:", len(out.Messages))
		for i, m := range out.Messages {
			t.Logf("  [%d] role=%s content=%v tool_calls=%d", i, m.Role, m.Content, len(m.ToolCalls))
		}
		t.FailNow()
	}
	asst := out.Messages[1]
	if asst.Role != "assistant" {
		t.Errorf("第 2 条应为 assistant，得到 %s", asst.Role)
	}
	if len(asst.ToolCalls) != 1 {
		t.Errorf("assistant 应携带 1 个 tool_call，得到 %d", len(asst.ToolCalls))
	}
	if asst.Content != "Let me check." {
		t.Errorf("assistant 正文丢失: %v", asst.Content)
	}
	// tool 消息必须紧跟在带 tool_calls 的 assistant 之后
	if out.Messages[2].Role != "tool" {
		t.Errorf("第 3 条应为 tool，得到 %s", out.Messages[2].Role)
	}
	if out.Messages[2].ToolCallID != "call_1" {
		t.Errorf("tool_call_id 错误: %s", out.Messages[2].ToolCallID)
	}
}

// custom 工具（Codex 的 apply_patch）：请求方向要把 input 原文包成 {"content":...}
func TestBuildChatRequest_CustomToolWrapped(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: a.py\n-a\n+b\n*** End Patch"
	req := &Request{
		Model: "deepseek-flash",
		Input: json.RawMessage(`[
			{"type":"custom_tool_call","call_id":"c1","name":"apply_patch",
			 "input":` + mustJSON(t, patch) + `}
		]`),
	}
	out, toolCtx, err := BuildChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !toolCtx.isCustom("apply_patch") {
		t.Error("虽然请求里没有 tools 定义，但 custom_tool_call 暗示了工具存在")
	}
	if len(out.Messages) != 1 || len(out.Messages[0].ToolCalls) != 1 {
		t.Fatalf("期望 1 条 assistant 带 1 个 tool_call，得到 %+v", out.Messages)
	}
	args := out.Messages[0].ToolCalls[0]["function"].(map[string]any)["arguments"].(string)
	var wrapped map[string]string
	if err := json.Unmarshal([]byte(args), &wrapped); err != nil {
		t.Fatalf("arguments 应为 JSON 对象: %v (raw=%s)", err, args)
	}
	if wrapped["content"] != patch {
		t.Errorf("patch 原文未正确包装:\n want %q\n got  %q", patch, wrapped["content"])
	}
}

// custom 工具的 schema 包装：freeform 工具的 format 文法要进 description
func TestCustomToolToChatTool_GrammarInDescription(t *testing.T) {
	tool := Tool{
		Kind:        ToolCustom,
		Name:        "apply_patch",
		Description: "Edit files.",
		Format:      json.RawMessage(`{"type":"grammar","syntax":"lark","definition":"start: patch"}`),
	}
	out := customToolToChatTool(tool, tool.Name)
	fn := out["function"].(map[string]any)

	desc := fn["description"].(string)
	if !strings.Contains(desc, "Edit files.") {
		t.Error("原 description 丢失")
	}
	if !strings.Contains(desc, "start: patch") || !strings.Contains(desc, "lark") {
		t.Errorf("文法定丢失，模型将无法产出正确格式: %q", desc)
	}

	params := fn["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	if _, ok := props["content"]; !ok {
		t.Error("缺少 content 属性：回程解包依赖这个字段名")
	}
	required := params["required"].([]string)
	if len(required) != 1 || required[0] != "content" {
		t.Errorf("required 应为 [content]，得到 %v", required)
	}
}

// namespace 扁平化
func TestFlattenNamespaceName(t *testing.T) {
	got := flattenNamespaceName("collaboration", "followup_task")
	if got != "collaboration__followup_task" {
		t.Errorf("want collaboration__followup_task, got %s", got)
	}
	// 空 namespace 不做前缀（custom 工具不参与 namespace 限定）
	if got := flattenNamespaceName("", "x"); got != "__x" {
		t.Errorf("空 namespace 行为: %s", got)
	}
}

// 超长工具名的哈希截断：两个长名不能撞成同一个
func TestFlattenNamespaceName_LongNameHashed(t *testing.T) {
	ns := strings.Repeat("verylongnamespace", 5)
	a := flattenNamespaceName(ns, "tool_a")
	b := flattenNamespaceName(ns, "tool_b")
	if len(a) > chatToolNameMaxLen {
		t.Errorf("未截断到上限: len=%d %q", len(a), a)
	}
	if a == b {
		t.Errorf("不同工具撞名: %q", a)
	}
	// 同名必须稳定（重复调用结果一致，否则模型调用的名字对不上）
	if a != flattenNamespaceName(ns, "tool_a") {
		t.Error("截断结果不稳定")
	}
}

// 解包容错：模型可能不按 schema 产出
func TestUnwrapCustomArguments(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"content":"*** Begin Patch\n*** End Patch"}`, "*** Begin Patch\n*** End Patch"},
		{`*** Begin Patch`, `*** Begin Patch`}, // 裸文本：原样返回
		{`{"other":"x"}`, `{"other":"x"}`},     // 无 content 键：原样返回
		{``, ``},                               // 空
		{`{"content":""}`, ``},                 // 空 content
	}
	for _, c := range cases {
		if got := unwrapCustomArguments(c.in); got != c.want {
			t.Errorf("unwrap(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// developer 角色归一：上游对 role 做白名单校验，developer 会 400
func TestNormalizeRole(t *testing.T) {
	cases := map[string]string{
		"developer": "system",
		"DEVELOPER": "system",
		"system":    "system",
		"user":      "user",
		"assistant": "assistant",
		"tool":      "tool",
		"":          "user",
	}
	for in, want := range cases {
		if got := normalizeRole(in); got != want {
			t.Errorf("normalizeRole(%q) = %q, want %q", in, got, want)
		}
	}
}

// reasoning.effort 映射
func TestReasoningEffortMapping(t *testing.T) {
	req := &Request{
		Model:     "deepseek-flash",
		Input:     json.RawMessage(`"hi"`),
		Reasoning: &Reasoning{Effort: "high"},
	}
	out, _, err := BuildChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if out.ReasoningEffort != "high" {
		t.Errorf("want high, got %s", out.ReasoningEffort)
	}
	// 未知档位不猜，透传交给既有管线的按模型降级逻辑
	req.Reasoning.Effort = "ultra"
	out2, _, _ := BuildChatRequest(req)
	if out2.ReasoningEffort != "ultra" {
		t.Errorf("未知档位应透传，得到 %s", out2.ReasoningEffort)
	}
}

// input 为纯字符串的简写形态
func TestBuildChatRequest_StringInput(t *testing.T) {
	req := &Request{Model: "m", Input: json.RawMessage(`"just a string"`)}
	out, _, err := BuildChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 || out.Messages[0].Content != "just a string" {
		t.Errorf("字符串 input 未正确处理: %+v", out.Messages)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tool_search 必须转成普通 function。
//
// 背景：type:"tool_search" 不在 Chat Completions 白名单里，原样透传时上游
// **静默忽略**它——实测直接问 Codex，它回答"我没有 tool_search 这个工具"。
// 转成 function 后模型才能看见并调用，进而触发 Codex 的客户端搜索。
func TestToolSearchConvertedToFunction(t *testing.T) {
	req := &Request{
		Model: "m",
		Input: json.RawMessage(`"find tools"`),
		Tools: []Tool{{
			Kind:        ToolSearch,
			Execution:   "client",
			Description: "# Tool discovery\nSearches deferred tools.",
			Raw:         json.RawMessage(`{"type":"tool_search","execution":"client","description":"# Tool discovery\nSearches deferred tools.","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}`),
		}},
	}
	out, ctx, err := BuildChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("tools 数 = %d, want 1", len(out.Tools))
	}
	tool := out.Tools[0]
	if tool["type"] != "function" {
		t.Errorf("type = %v, want function（原样透传会被上游忽略）", tool["type"])
	}
	fn, _ := tool["function"].(map[string]any)
	if fn["name"] != "tool_search" {
		t.Errorf("name = %v, want tool_search", fn["name"])
	}
	// description 必须保留：内含可用工具源清单，丢了模型不会用这个工具
	if !strings.Contains(fn["description"].(string), "Tool discovery") {
		t.Errorf("description 丢失: %v", fn["description"])
	}
	if _, ok := ctx.chatNameToSpec["tool_search"]; !ok {
		t.Error("未登记 tool_search，回程无法还原成 tool_search_call")
	}
}

// tool_search_output 里的新工具必须被收集进 tools。
//
// 否则搜索结果等于白给：模型看到工具描述却无法调用（不在 tools 列表里）。
func TestCollectToolSearchOutputTools(t *testing.T) {
	req := &Request{
		Model: "m",
		Input: json.RawMessage(`[
			{"type":"tool_search_call","call_id":"s1","execution":"client","arguments":{"query":"docs"}},
			{"type":"tool_search_output","call_id":"s1","status":"completed","execution":"client",
			 "tools":[{"type":"function","name":"context7_docs","description":"Fetch docs",
			   "parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}
		]`),
	}
	out, ctx, err := BuildChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	// 发现的工具应出现在 tools 里
	var found bool
	for _, tool := range out.Tools {
		if fn, ok := tool["function"].(map[string]any); ok && fn["name"] == "context7_docs" {
			found = true
		}
	}
	if !found {
		t.Errorf("tool_search 发现的工具未被加入 tools；模型无法调用它。tools=%v", out.Tools)
	}
	if _, ok := ctx.chatNameToSpec["context7_docs"]; !ok {
		t.Error("发现的工具未登记到上下文")
	}
}

// tool_search_output 要作为 tool 消息回给上游，模型才知道搜到了什么
func TestToolSearchOutputBecomesToolMessage(t *testing.T) {
	req := &Request{
		Model: "m",
		Input: json.RawMessage(`[
			{"type":"tool_search_call","call_id":"s1","execution":"client","arguments":{"query":"x"}},
			{"type":"tool_search_output","call_id":"s1","status":"completed","execution":"client",
			 "tools":[{"type":"function","name":"t1","description":"d1"}]}
		]`),
	}
	out, _, err := BuildChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var toolMsg *ChatMessage
	for i := range out.Messages {
		if out.Messages[i].Role == "tool" {
			toolMsg = &out.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatalf("未产出 tool 消息，模型看不到搜索结果: %+v", out.Messages)
	}
	if toolMsg.ToolCallID != "s1" {
		t.Errorf("tool_call_id = %s, want s1（关联搜索调用）", toolMsg.ToolCallID)
	}
	content, _ := toolMsg.Content.(string)
	if !strings.Contains(content, "t1") {
		t.Errorf("搜索结果内容里没有工具名: %q", content)
	}
}

// tool_search_call 作为历史 item 时，要转成 Chat 的 tool_call（不是 custom/function）
func TestToolSearchCallItemConverted(t *testing.T) {
	req := &Request{
		Model: "m",
		Input: json.RawMessage(`[
			{"type":"tool_search_call","call_id":"s1","execution":"client","arguments":{"query":"docs","limit":3}},
			{"type":"tool_search_output","call_id":"s1","status":"completed","execution":"client","tools":[]}
		]`),
	}
	out, _, err := BuildChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range out.Messages {
		for _, tc := range m.ToolCalls {
			fn, _ := tc["function"].(map[string]any)
			if fn["name"] == "tool_search" {
				found = true
				// arguments 必须是 JSON 字符串（Chat 规范）
				args, _ := fn["arguments"].(string)
				if !strings.Contains(args, "docs") {
					t.Errorf("arguments 丢失查询内容: %q", args)
				}
			}
		}
	}
	if !found {
		t.Errorf("tool_search_call 未转成 Chat tool_call: %+v", out.Messages)
	}
}

// additional_tools input item：新版 Codex（0.154+）把工具定义放进 input 的
// additional_tools item（namespace + custom），顶层 tools 字段为空。
// 转换器必须从中收集工具，否则上游收不到任何工具定义——模型只能用文字
// "宣布要调工具"，调用永远不发生（2026-09-13 线上事故，抓包定位）。
func TestBuildChatRequest_AdditionalToolsItem(t *testing.T) {
	req := &Request{
		Model: "gpt-6-astra",
		Input: json.RawMessage(`[
			{"type":"additional_tools","id":"at_1","role":"developer","tools":[
				{"type":"namespace","name":"functions","description":"","tools":[
					{"type":"custom","name":"exec","description":"Run JS"}
				]},
				{"type":"function","name":"get_weather","description":"天气",
				 "parameters":{"type":"object","properties":{"city":{"type":"string"}}}}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"现在几点？"}]}
		]`),
	}
	out, _, err := BuildChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) == 0 {
		t.Fatal("additional_tools 里的工具未被收集：上游将收不到任何工具定义")
	}
	names := map[string]bool{}
	for _, tl := range out.Tools {
		fn, _ := tl["function"].(map[string]any)
		names[fn["name"].(string)] = true
	}
	// namespace 扁平化 + 顶层 function 都要进工具集
	for _, want := range []string{"functions__exec", "get_weather"} {
		if !names[want] {
			t.Errorf("工具 %q 未出现在转换结果: %v", want, names)
		}
	}
	// 嵌套 custom 必须带 content 参数（否则模型只能传 {}，工具必然失败）
	for _, tl := range out.Tools {
		fn, _ := tl["function"].(map[string]any)
		if fn["name"] != "functions__exec" {
			continue
		}
		params, _ := fn["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		if _, ok := props["content"]; !ok {
			t.Errorf("嵌套 custom 工具缺 content 参数，模型将无法传入输入: %v", fn)
		}
		// required 在内存里是 []string，经 JSON 往返后是 []any，两种都要认
		switch r := params["required"].(type) {
		case []string:
			if len(r) == 0 {
				t.Errorf("content 应为必填参数: %v", params)
			}
		case []any:
			if len(r) == 0 {
				t.Errorf("content 应为必填参数: %v", params)
			}
		default:
			t.Errorf("content 应为必填参数: %v", params)
		}
	}
	// assistant 消息不受影响
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
		t.Errorf("additional_tools 不应产出消息: %+v", out.Messages)
	}
}
