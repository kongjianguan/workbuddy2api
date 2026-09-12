package server

import (
	"io"
	"net/http"
	"time"

	"workbuddy2api/internal/responses"
	"workbuddy2api/internal/upstream"
)

// responsesEndpoint 处理 POST /v1/responses（OpenAI Responses API）。
//
// 存在理由：Codex CLI 已移除 wire_api="chat"，只发 Responses 请求；而上游
// CodeBuddy 只接受 Chat Completions。此端点把请求转成 Chat 后复用网关既有的
// 完整链路（选号/轮转/冷却/提示词改写/thinking 注入/指纹脱敏），再把结果
// 转回 Responses 形态。
//
// 与 /v1/chat/completions 的唯一区别是协议层；账号治理逻辑完全共享。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, _, ok := h.readChatBody(w, r)
	if !ok {
		return
	}

	req, chatBody, toolCtx, ok := responses.Convert(w, body, h.cfg.MaxBodyBytes)
	if !ok {
		return
	}

	// 转换后的 Chat 请求体送入既有链路。peek 从转换结果取：stream 与 model
	// 都以 Chat 请求为准（model 可能被转换层保持原样，stream 恒为客户端请求值）。
	peek := chatPeek{Stream: req.Stream, Model: req.Model}

	hooks := chatExecHooks{
		OnStream: func(w http.ResponseWriter, rc io.ReadCloser, since time.Time) (time.Duration, int, error) {
			tr := responses.NewStreamTranslator(w, toolCtx, req.Model)
			start := time.Now()
			err := tr.Run(rc)
			return time.Since(start), tr.OutputTokens(), err
		},
		OnSync: func(w http.ResponseWriter, resp map[string]any) (int, error) {
			out := responses.BuildResponse(resp, toolCtx, req.Model)
			responses.WriteJSON(w, http.StatusOK, out)
			return responses.UsageTokens(out), nil
		},
	}
	h.executeChat(w, chatBody, peek, hooks)
}

// 保证 upstream 包被引用（Aggregate 在 hooks 外部路径仍需要）。
var _ = upstream.Aggregate
