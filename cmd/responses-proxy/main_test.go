package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResponsesProxyConvertsStream(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"model\":\"glm-5.3-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	u, _ := url.Parse(up.URL)
	s := &server{
		upstream:        u,
		watchdogTimeout: 2 * time.Second,
		maxRetries:      1,
		client:          &http.Client{Timeout: 5 * time.Second},
	}

	body := `{
	  "model":"glm-5.3-flash","stream":true,
	  "input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]
	}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	s.responses(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path=%s want /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("auth not forwarded: %q", gotAuth)
	}
	var chat map[string]any
	if err := json.Unmarshal([]byte(gotBody), &chat); err != nil {
		t.Fatalf("chat body: %v", err)
	}
	if chat["model"] != "glm-5.3-flash" {
		t.Errorf("model=%v", chat["model"])
	}
	out := rec.Body.String()
	if !strings.Contains(out, "event: response.created") {
		t.Errorf("missing response.created:\n%s", out)
	}
	if !strings.Contains(out, "response.output_text.delta") && !strings.Contains(out, `"hi"`) {
		t.Errorf("missing text delta:\n%s", out)
	}
	if !strings.Contains(out, "event: response.completed") {
		t.Errorf("missing response.completed:\n%s", out)
	}
}

func TestApplyModelAliasRewritesChatBody(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var chat map[string]any
		_ = json.Unmarshal(b, &chat)
		gotModel, _ = chat["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"glm-5.3-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	s := &server{
		upstream:        u,
		client:          &http.Client{Timeout: 5 * time.Second},
		watchdogTimeout: 2 * time.Second,
		maxRetries:      1,
		aliases:         map[string]string{"gpt-5.6-sol": "deepseek-v4.1-flash"},
	}
	body := `{"model":"gpt-5.6-sol","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.responses(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotModel != "deepseek-v4.1-flash" {
		t.Errorf("upstream model=%q want deepseek-v4.1-flash", gotModel)
	}
}

func TestResponsesProxyWatchdogSilentRetryOnStall(t *testing.T) {
	var attempts int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			// 第一次尝试：模拟上游恶劣行为（秒回 200，挂起 1 秒不发任何数据）
			time.Sleep(500 * time.Millisecond)
			return
		}
		// 第二次重试：正常下发数据
		_, _ = io.WriteString(w, "data: {\"id\":\"c2\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"recovered\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"c2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	u, _ := url.Parse(up.URL)
	s := &server{
		upstream:        u,
		watchdogTimeout: 100 * time.Millisecond, // 测试环境设为 100ms
		maxRetries:      1,
		client:          &http.Client{Timeout: 5 * time.Second},
	}

	body := `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.responses(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if !strings.Contains(rec.Body.String(), "recovered") {
		t.Fatalf("expected response to contain 'recovered', got %s", rec.Body.String())
	}
}

func TestResponsesProxyWatchdogTimeoutExhausted(t *testing.T) {
	var attempts int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		// 每次尝试均假死
		time.Sleep(300 * time.Millisecond)
	}))
	defer up.Close()

	u, _ := url.Parse(up.URL)
	s := &server{
		upstream:        u,
		watchdogTimeout: 50 * time.Millisecond,
		maxRetries:      1, // 最多 2 次
		client:          &http.Client{Timeout: 5 * time.Second},
	}

	body := `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.responses(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d want 504 body=%s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("expected 2 attempts before giving up, got %d", attempts)
	}
	if !strings.Contains(rec.Body.String(), "upstream stalled") {
		t.Fatalf("expected error message to mention upstream stalled, got %s", rec.Body.String())
	}
}

func TestChatCompletionsAliasAndWatchdog(t *testing.T) {
	var attempts int32
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		b, _ := io.ReadAll(r.Body)
		var chat map[string]any
		_ = json.Unmarshal(b, &chat)
		gotModel, _ = chat["model"].(string)

		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			time.Sleep(300 * time.Millisecond)
			return
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	u, _ := url.Parse(up.URL)
	s := &server{
		upstream:        u,
		watchdogTimeout: 50 * time.Millisecond,
		maxRetries:      1,
		aliases:         map[string]string{"gpt-5.6-sol": "deepseek-v4.1-flash"},
		client:          &http.Client{Timeout: 5 * time.Second},
	}

	body := `{"model":"gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":"ping"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.chatCompletions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotModel != "deepseek-v4.1-flash" {
		t.Errorf("model alias not applied, got=%s want=deepseek-v4.1-flash", gotModel)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("missing pong in body: %s", rec.Body.String())
	}
}

// TestUpstreamHeaderTimeoutIsWatchdogAligned 断言「等待上游响应头」的上限与看门狗对齐。
//
// 这个上限覆盖面是两道看门狗够不到的阶段：`client.Do` 返回前没有 body 可读，
// -watchdog-timeout 与 -stream-idle-timeout 都不在计时。上游连响应头都不给时
// （排队静默 / 半开连接），它是唯一的上限。
//
// 上限必须是有限值且不宽于网关自己的 header_timeout：链路是代理 → manager →
// 网关 → 腾讯，代理放得比网关宽，只会把「中间那跳卡住」的情形拖长。
func TestUpstreamHeaderTimeoutIsWatchdogAligned(t *testing.T) {
	c := newUpstreamClient(upstreamHeaderTimeout)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", c.Transport)
	}
	if tr.ResponseHeaderTimeout != upstreamHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout=%v want %v", tr.ResponseHeaderTimeout, upstreamHeaderTimeout)
	}
	if upstreamHeaderTimeout <= 0 {
		t.Fatal("等头超时必须有限，否则该阶段无上限")
	}
	// 网关侧 header_timeout_seconds 配的是 45s，代理不得超过它。
	if upstreamHeaderTimeout > 45*time.Second {
		t.Errorf("等头超时 %v 宽于网关的 45s，中间跳卡住时会被拖长", upstreamHeaderTimeout)
	}
	if c.Timeout != 0 {
		t.Errorf("流式聊天不应设总时长上限，got %v", c.Timeout)
	}
}

// TestHeaderTimeoutIsStallError 断言等头超时会被判为可静默重试。
//
// 若该错误落不进 isStallError，代理会把它当成终态错误直接回给客户端，
// 「换号重试」在看门狗之前的阶段就断了。
func TestHeaderTimeoutIsStallError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 拿到请求但永不回响应头。
		time.Sleep(5 * time.Second)
	}))
	defer up.Close()

	c := newUpstreamClient(150 * time.Millisecond)
	req := httptest.NewRequest("POST", up.URL, strings.NewReader("{}"))
	_, err := c.Do(req)
	if err == nil {
		t.Fatal("上游不给响应头时必须超时")
	}
	if !isStallError(err) {
		t.Errorf("等头超时应可静默重试，err=%v", err)
	}
}

// TestHeaderTimeoutTriggersSilentRetry 端到端断言：第一次上游只挂起不回响应头，
// 第二次正常返回时，客户端最终拿到成功响应且代理确实重试过。
func TestHeaderTimeoutTriggersSilentRetry(t *testing.T) {
	var attempts int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			// 不回响应头，让传输层超时先于看门狗生效。
			time.Sleep(2 * time.Second)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"recovered\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	u, _ := url.Parse(up.URL)
	s := &server{
		upstream:        u,
		watchdogTimeout: 5 * time.Second, // 远大于等头超时，确保是传输层先超时
		idleTimeout:     5 * time.Second,
		maxRetries:      1,
		client:          newUpstreamClient(200 * time.Millisecond),
	}

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"ping"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.chatCompletions(rec, req)

	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("等头超时未触发静默重试，attempts=%d want 2", got)
	}
	if !strings.Contains(rec.Body.String(), "recovered") {
		t.Fatalf("重试后的内容未送达客户端: %s", rec.Body.String())
	}
}
