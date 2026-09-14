package main

import (
	"encoding/json"
	"testing"
)

func callsOf(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var msgs []map[string]any
	if err := json.Unmarshal(root["messages"], &msgs); err != nil {
		t.Fatalf("messages: %v", err)
	}
	return msgs
}

// TestSanitizeToolCallIDsBindsBothSides 覆盖上游实测约束：
// 两侧都为空 / 只补一侧 / 两侧不一致 都会被 400 拒收，必须两侧都存在且相等。
func TestSanitizeToolCallIDsBindsBothSides(t *testing.T) {
	body := []byte(`{
	  "model":"m",
	  "messages":[
	    {"role":"user","content":"go"},
	    {"role":"assistant","content":"","tool_calls":[
	        {"id":"","type":"function","function":{"name":"bash","arguments":"{}"}}
	    ]},
	    {"role":"tool","tool_call_id":"","content":"[stderr] boom"},
	    {"role":"assistant","content":"","tool_calls":[
	        {"id":"","type":"function","function":{"name":"bash","arguments":"{}"}}
	    ]},
	    {"role":"tool","tool_call_id":"","content":"ok"}
	  ]
	}`)

	fixed, changed := sanitizeToolCallIDs(body)
	if !changed {
		t.Fatal("应检测到需要修复的空 id，却报告未改动")
	}
	msgs := callsOf(t, fixed)

	var declared []string
	for i, m := range msgs {
		if role, _ := m["role"].(string); role == "assistant" {
			tcs, _ := m["tool_calls"].([]any)
			for _, raw := range tcs {
				tc, _ := raw.(map[string]any)
				id, _ := tc["id"].(string)
				if id == "" {
					t.Errorf("message[%d] assistant.tool_calls[].id 仍为空", i)
					continue
				}
				declared = append(declared, id)
			}
		}
	}

	var referenced []string
	for i, m := range msgs {
		if role, _ := m["role"].(string); role == "tool" {
			id, _ := m["tool_call_id"].(string)
			if id == "" {
				t.Errorf("message[%d] tool.tool_call_id 仍为空", i)
				continue
			}
			referenced = append(referenced, id)
		}
	}

	if len(declared) != len(referenced) {
		t.Fatalf("声明数 %d != 引用数 %d", len(declared), len(referenced))
	}
	for i := range declared {
		if declared[i] != referenced[i] {
			t.Errorf("第 %d 组不配对: 声明 %q vs 引用 %q", i, declared[i], referenced[i])
		}
	}
}

// TestSanitizeToolCallIDsRebindsDanglingReference 覆盖 tool 消息引用了不存在 id 的情况。
func TestSanitizeToolCallIDsRebindsDanglingReference(t *testing.T) {
	body := []byte(`{
	  "model":"m",
	  "messages":[
	    {"role":"assistant","content":"","tool_calls":[
	        {"id":"call_real","type":"function","function":{"name":"bash","arguments":"{}"}}
	    ]},
	    {"role":"tool","tool_call_id":"call_ghost","content":"orphan"}
	  ]
	}`)

	fixed, changed := sanitizeToolCallIDs(body)
	if !changed {
		t.Fatal("悬空 tool_call_id 应被重绑")
	}
	msgs := callsOf(t, fixed)
	id, _ := msgs[1]["tool_call_id"].(string)
	if id != "call_real" {
		t.Errorf("悬空引用应重绑到 call_real，实际 %q", id)
	}
}

// TestSanitizeToolCallIDsLeavesValidHistoryUntouched 合法历史必须原样返回。
func TestSanitizeToolCallIDsLeavesValidHistoryUntouched(t *testing.T) {
	body := []byte(`{
	  "model":"m",
	  "messages":[
	    {"role":"assistant","content":"","tool_calls":[
	        {"id":"call_1","type":"function","function":{"name":"bash","arguments":"{}"}}
	    ]},
	    {"role":"tool","tool_call_id":"call_1","content":"ok"}
	  ]
	}`)

	out, changed := sanitizeToolCallIDs(body)
	if changed {
		t.Fatal("合法历史不应被改动")
	}
	if string(out) != string(body) {
		t.Error("未改动时应原样返回输入字节")
	}
}

// TestSanitizeToolCallIDsHandlesMissingMessages 无 messages 字段时安全返回。
func TestSanitizeToolCallIDsHandlesMissingMessages(t *testing.T) {
	body := []byte(`{"model":"m","input":[]}`)
	out, changed := sanitizeToolCallIDs(body)
	if changed {
		t.Fatal("无 messages 不应报告改动")
	}
	if string(out) != string(body) {
		t.Error("应原样返回")
	}
}
