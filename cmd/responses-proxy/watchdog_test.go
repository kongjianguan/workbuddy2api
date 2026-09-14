package main

import (
	"bufio"
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

// TestNoDataLossAcrossPeekWithMultipleToolCalls 是「首包探测吞数据」的回归测试。
//
// 早期实现让探测 goroutine 先 Read 一段就返回，而那个 goroutine 并未停止，
// 继续把后续数据抢进无人消费的 channel 并丢弃。当一轮里有多个工具调用时，
// 第二个调用携带 id 的首帧会被吞掉，客户端合并出的 tool_call 缺 id，
// 于是报 `Expected 'id' to be a string.`
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
		write("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		time.Sleep(30 * time.Millisecond)
		write("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_AAA\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"\"}}]}}]}\n\n")
		time.Sleep(30 * time.Millisecond)
		write("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"command\\\":\\\"ls\\\"}\"}}]}}]}\n\n")
		time.Sleep(30 * time.Millisecond)
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
		idleTimeout:     2 * time.Second,
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

// TestHeartbeatDoesNotKeepStalledStreamAlive 是「心跳续命」的回归测试。
//
// 上游在排队/思考期间会持续发 `: heartbeat` 与只带 role 的空帧。若看门狗把
// 「收到任意字节」当作存活证据，这些心跳会无限续命，看门狗永不触发，客户端
// 一直挂到上游自身的空闲阈值（实测可达 5 分钟）才断开。
//
// 本测试让上游只发心跳、从不发真实内容，断言看门狗仍会按首包阈值掐断。
func TestHeartbeatDoesNotKeepStalledStreamAlive(t *testing.T) {
	var heartbeats int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		// 每 40ms 发一次心跳与空 role 帧，持续远超看门狗阈值
		for i := 0; i < 40; i++ {
			_, _ = io.WriteString(w, ": heartbeat\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
			if fl != nil {
				fl.Flush()
			}
			atomic.AddInt32(&heartbeats, 1)
			time.Sleep(40 * time.Millisecond)
		}
	}))
	defer up.Close()

	u, _ := url.Parse(up.URL)
	s := &server{
		upstream:        u,
		watchdogTimeout: 250 * time.Millisecond,
		idleTimeout:     250 * time.Millisecond,
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
	case <-time.After(3 * time.Second):
		t.Fatalf("心跳把看门狗续命了：只发心跳的流未被掐断（已发 %d 次心跳）", atomic.LoadInt32(&heartbeats))
	}

	if got := atomic.LoadInt32(&heartbeats); got > 15 {
		t.Errorf("看门狗触发过晚：期间上游发了 %d 次心跳，说明心跳仍在续命", got)
	}
}

// TestIdleWatchdogAbortsAfterContentThenSilence 验证“先出内容、再静默”的中途闸门。
func TestIdleWatchdogAbortsAfterContentThenSilence(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		// 有内容之后只发心跳，不再推进
		for i := 0; i < 40; i++ {
			_, _ = io.WriteString(w, ": heartbeat\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(40 * time.Millisecond)
		}
	}))
	defer up.Close()

	u, _ := url.Parse(up.URL)
	s := &server{
		upstream:        u,
		watchdogTimeout: 150 * time.Millisecond,
		idleTimeout:     250 * time.Millisecond,
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
	case <-time.After(3 * time.Second):
		t.Fatal("中途静默未被掐断：心跳续命导致客户端会一直挂着")
	}

	if !strings.Contains(rec.Body.String(), "hi") {
		t.Error("已到达的内容应当透传给客户端")
	}
}

// TestFrameProgressClassification 固定「什么算内容推进」的判定。
func TestFrameProgressClassification(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{": heartbeat", false},
		{"", false},
		{"data: [DONE]", true},
		{`data: {"choices":[{"delta":{"role":"assistant"}}]}`, false},
		{`data: {"choices":[{"delta":{"content":"x"}}]}`, true},
		{`data: {"choices":[{"delta":{"reasoning_content":"x"}}]}`, true},
		{`data: {"choices":[{"delta":{"tool_calls":[{"index":0}]}}]}`, true},
		{`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, false},
		{`data: {"choices":[],"usage":{"completion_tokens":0}}`, false},
	}
	for _, c := range cases {
		if got := frameProgress(c.line); got != c.want {
			t.Errorf("frameProgress(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

// TestNoDataLossWithHeartbeatsInterleaved 心跳与内容交错时不得丢内容。
//
// 心跳出现在 SSE 行边界上，行扫描残留处理不当会吃掉相邻行。
func TestNoDataLossWithHeartbeatsInterleaved(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, s)
			if fl != nil {
				fl.Flush()
			}
		}
		write(": heartbeat\n\n")
		write("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"AAA\"}}]}\n\n")
		write(": heartbeat\n\n")
		write("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"BBB\"}}]}\n\n")
		write(": heartbeat\n\n")
		write("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"CCC\"}}]}\n\n")
		write("data: [DONE]\n\n")
	}))
	defer up.Close()

	u, _ := url.Parse(up.URL)
	s := &server{
		upstream:        u,
		watchdogTimeout: 2 * time.Second,
		idleTimeout:     2 * time.Second,
		maxRetries:      0,
		client:          &http.Client{Timeout: 20 * time.Second},
	}

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"go"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.chatCompletions(rec, req)

	out := rec.Body.String()
	for _, want := range []string{"AAA", "BBB", "CCC", "[DONE]"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q（心跳交错导致丢内容）:\n%s", want, out)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
