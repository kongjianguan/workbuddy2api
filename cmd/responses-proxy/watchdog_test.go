package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestNoDataLossAcrossPeekWithMultipleToolCalls 是「首包探测吞数据」的回归测试。
//
// 早期实现让探测 goroutine 先 Read 一段就返回，而那个 goroutine 并未停止，
// 继续把后续数据抢进无人消费的 channel 并丢弃。当一轮里有多个工具调用时，
// 第二个调用携带 id 的首帧会被吞掉，客户端合并出的 tool_call 缺 id，
// 于是报 `Expected 'id' to be a string.`。
//
// 本测试构造带延迟的多段 tool_calls 流，断言客户端收到**全部** id 帧。
func TestNoDataLossAcrossPeekWithMultipleToolCalls(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, s)
			if fl != nil {
				fl.Flush()
			}
		}
		// 首帧：空 role 占位（触发探测等待真实内容）
		write("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		time.Sleep(30 * time.Millisecond)
		// 真实内容：第一个工具调用的 id 帧
		write("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_AAA\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"\"}}]}}]}\n\n")
		time.Sleep(30 * time.Millisecond)
		// 第一个调用的参数分片
		write("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"command\\\":\\\"ls\\\"}\"}}]}}]}\n\n")
		time.Sleep(30 * time.Millisecond)
		// 第二个工具调用的 id 帧——这一帧在旧实现里会被探测 goroutine 吞掉
		write("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_BBB\",\"type\":\"function\",\"function\":{\"name\":\"Read\",\"arguments\":\"\"}}]}}]}\n\n")
		time.Sleep(30 * time.Millisecond)
		write("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"{\\\"path\\\":\\\"a.txt\\\"}\"}}]}}]}\n\n")
		time.Sleep(30 * time.Millisecond)
		write("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		write("data: [DONE]\n\n")
	}))
	defer up.Close()

	u, _ := url.Parse(up.URL)
	s := &server{
		upstream:        u,
		watchdogTimeout: 2 * time.Second,
		maxRetries:      0,
		client:          &http.Client{Timeout: 20 * time.Second},
	}

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"go"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.chatCompletions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 逐帧解析，确认两个工具调用的 id 都到达客户端
	ids := map[string]bool{}
	names := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			cm, _ := c.(map[string]any)
			delta, _ := cm["delta"].(map[string]any)
			tcs, _ := delta["tool_calls"].([]any)
			for _, tc := range tcs {
				tcm, _ := tc.(map[string]any)
				if id, ok := tcm["id"].(string); ok && id != "" {
					ids[id] = true
				}
				if fn, ok := tcm["function"].(map[string]any); ok {
					if n, ok := fn["name"].(string); ok && n != "" {
						names[n] = true
					}
				}
			}
		}
	}

	if !ids["call_AAA"] {
		t.Errorf("丢失第一个工具调用的 id 帧 (call_AAA); 收到 ids=%v", keysOf(ids))
	}
	if !ids["call_BBB"] {
		t.Errorf("丢失第二个工具调用的 id 帧 (call_BBB) —— 这正是 Expected 'id' to be a string. 的成因; 收到 ids=%v", keysOf(ids))
	}
	if !names["Bash"] || !names["Read"] {
		t.Errorf("工具名丢失: %v", keysOf(names))
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestStreamIdleWatchdogAbortsStalledStream 验证中途断流会被看门狗掐断。
func TestStreamIdleWatchdogAbortsStalledStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		// 先给真实内容，让探测通过
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		// 随后挂死，不再发送任何数据
		time.Sleep(1500 * time.Millisecond)
	}))
	defer up.Close()

	u, _ := url.Parse(up.URL)
	s := &server{
		upstream:        u,
		watchdogTimeout: 200 * time.Millisecond,
		maxRetries:      0,
		client:          &http.Client{Timeout: 20 * time.Second},
	}

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"go"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.chatCompletions(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1200 * time.Millisecond):
		t.Fatal("看门狗未在超时后掐断挂死的流，客户端会一直等下去")
	}
}
