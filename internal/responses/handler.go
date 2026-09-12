package responses

import (
	"encoding/json"
	"net/http"
)

// Convert 解析并转换 Responses 请求体，返回原始请求、转换后的 Chat 请求体
// 与工具上下文。ok=false 表示已写出错误响应。
//
// 单独导出而非内联在 server 包，是为了让转换逻辑可被独立测试，
// 也避免 server 包直接依赖 Responses 的结构体细节。
func Convert(w http.ResponseWriter, body []byte, maxBodyBytes int64) (*Request, []byte, *ToolContext, bool) {
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "解析 Responses 请求失败: "+err.Error())
		return nil, nil, nil, false
	}
	chatReq, toolCtx, err := BuildChatRequest(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return nil, nil, nil, false
	}
	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return nil, nil, nil, false
	}
	return &req, chatBody, toolCtx, true
}

// WriteJSON 写出 JSON 响应。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// UsageTokens 提取响应里的输出 token 数，供日志使用（缺失返回 -1）。
func UsageTokens(r *Response) int {
	if r == nil || r.Usage == nil {
		return -1
	}
	return r.Usage.OutputTokens
}

// OutputTokens 返回流式翻译器累计的输出 token 数（缺失返回 -1）。
func (t *StreamTranslator) OutputTokens() int {
	if t.usage == nil {
		return -1
	}
	return t.usage.OutputTokens
}

// writeError 写 OpenAI 风格的错误响应。
func writeError(w http.ResponseWriter, status int, code, msg string) {
	WriteJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
