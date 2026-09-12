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
	out := customToolToChatTool(tool)
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
