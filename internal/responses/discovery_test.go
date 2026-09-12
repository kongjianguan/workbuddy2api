package responses

import (
	"encoding/json"
	"testing"
)

// 用真实捕获的 tool_search_output 结构验证工具收集。
// 结构取自实际抓包：namespace 形态、带 defer_loading 标记。
func TestRealToolSearchOutputCollection(t *testing.T) {
	input := `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"search"}]},
		{"type":"tool_search_call","call_id":"s1","execution":"client","arguments":{"query":"documentation"}},
		{"type":"tool_search_output","call_id":"s1","status":"completed","execution":"client",
		 "tools":[
		   {"type":"namespace","name":"mcp__Context7","description":"Docs","tools":[
		     {"name":"query_docs","description":"Query docs","parameters":{"type":"object","properties":{"q":{"type":"string"}}}},
		     {"name":"resolve_library_id","description":"Resolve","parameters":{"type":"object","properties":{}}}
		   ]},
		   {"type":"namespace","name":"mcp__anysearch","description":"Search","tools":[
		     {"name":"extract","description":"Extract","parameters":{"type":"object","properties":{}}}
		   ]}
		 ]}
	]`
	req := &Request{Model: "m", Input: json.RawMessage(input)}
	out, ctx, err := BuildChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"mcp__Context7__query_docs",
		"mcp__Context7__resolve_library_id",
		"mcp__anysearch__extract",
	}
	got := map[string]bool{}
	for _, tool := range out.Tools {
		if fn, ok := tool["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok {
				got[n] = true
			}
		}
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("缺失发现的工具 %s；模型无法调用它", w)
		}
	}
	t.Logf("tools 总数 = %d", len(out.Tools))

	// 回程必须能还原 namespace
	spec, ok := ctx.chatNameToSpec["mcp__Context7__query_docs"]
	if !ok {
		t.Fatal("未登记发现的工具，回程无法还原")
	}
	if spec.Kind != ToolNamespace || spec.Namespace != "mcp__Context7" || spec.Name != "query_docs" {
		t.Errorf("登记信息错误: %+v", spec)
	}

	// 搜索结果要作为 tool 消息回给模型
	var sawToolMsg bool
	for _, m := range out.Messages {
		if m.Role == "tool" && m.ToolCallID == "s1" {
			sawToolMsg = true
		}
	}
	if !sawToolMsg {
		t.Error("搜索结果未作为 tool 消息回给模型")
	}
}

// 重复发现的工具不应重复加入（Codex 每轮重发全量 tools + 搜索历史）
func TestDiscoveredToolNoDuplicate(t *testing.T) {
	input := `[
		{"type":"tool_search_output","call_id":"s1","tools":[
		  {"type":"function","name":"dup_tool","description":"d","parameters":{"type":"object","properties":{}}}
		]},
		{"type":"tool_search_output","call_id":"s2","tools":[
		  {"type":"function","name":"dup_tool","description":"d","parameters":{"type":"object","properties":{}}}
		]}
	]`
	req := &Request{Model: "m", Input: json.RawMessage(input)}
	out, _, err := BuildChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, tool := range out.Tools {
		if fn, ok := tool["function"].(map[string]any); ok && fn["name"] == "dup_tool" {
			n++
		}
	}
	if n > 1 {
		t.Errorf("工具重复加入 %d 次；上游会看到重复定义", n)
	}
}
