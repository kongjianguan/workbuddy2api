package main

import (
	"encoding/json"
	"fmt"
)

// sanitizeToolCallIDs 修复历史消息里不成对的工具调用 id。
//
// 客户端的会话历史里偶尔会留下 id 为空串的工具调用：assistant.tool_calls[].id
// 与紧随其后 role:"tool" 的 tool_call_id 双双为 ""（客户端某轮工具调用失败后
// 记录下来的残迹）。上游模型提供方会以 400 model_param_invalid 拒收这类请求。
//
// 实测确认的约束（逐变体重放真实请求得出）：
//   - 两侧都是空串      -> 400
//   - 只补一侧          -> 400
//   - 两侧都补但不相等  -> 400
//   - 两侧都补且相等    -> 200
//
// 因此修复必须保证「有且相等」：空 id 就地生成，随后按顺序绑定到引用它的
// tool 消息；引用了解析不出来的 id 的 tool 消息也一并重绑。
//
// 返回修复后的请求体与是否发生改动。
func sanitizeToolCallIDs(body []byte) ([]byte, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return body, false
	}
	rawMsgs, ok := root["messages"]
	if !ok {
		return body, false
	}
	var msgs []map[string]any
	if json.Unmarshal(rawMsgs, &msgs) != nil {
		return body, false
	}

	changed := false
	seq := 0
	// declared 收集全部已声明的 tool_call id，用于判断 tool 消息的引用能否解析。
	declared := map[string]bool{}
	// pending 是最近一条 assistant 声明、尚未被 tool 消息消费的 id，按声明顺序。
	var pending []string

	for _, m := range msgs {
		switch roleOf(m) {
		case "assistant":
			tcs, _ := m["tool_calls"].([]any)
			if len(tcs) == 0 {
				continue
			}
			pending = pending[:0]
			for _, raw := range tcs {
				tc, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				id, _ := tc["id"].(string)
				if id == "" {
					seq++
					id = fmt.Sprintf("call_autofix_%d", seq)
					tc["id"] = id
					changed = true
				}
				declared[id] = true
				pending = append(pending, id)
			}

		case "tool":
			id, _ := m["tool_call_id"].(string)
			if id != "" && declared[id] {
				pending = removeString(pending, id)
				continue
			}
			// 空串或引用不存在的 id：按顺序绑到最近声明且未消费的 id 上。
			if len(pending) > 0 {
				m["tool_call_id"] = pending[0]
				pending = pending[1:]
				changed = true
			} else {
				seq++
				newID := fmt.Sprintf("call_autofix_%d", seq)
				m["tool_call_id"] = newID
				declared[newID] = true
				changed = true
			}
		}
	}

	if !changed {
		return body, false
	}

	outMsgs, err := json.Marshal(msgs)
	if err != nil {
		return body, false
	}
	root["messages"] = outMsgs
	out, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	return out, true
}

func roleOf(m map[string]any) string {
	r, _ := m["role"].(string)
	return r
}

func removeString(s []string, v string) []string {
	for i, x := range s {
		if x == v {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}
