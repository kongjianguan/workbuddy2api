package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrStreamStalled 表示看门狗超时内没有收到任何**真实内容**推进。
	ErrStreamStalled = errors.New("upstream stream stalled: no content within watchdog timeout")
	// ErrEmptyStream 表示上游返回 200 后未发送任何数据即关闭。
	ErrEmptyStream = errors.New("upstream closed stream without sending data")
)

// frameProgress 判定一个完整 SSE 行是否代表「真实内容推进」。
//
// 只有正文 / 思考 / 工具调用 / [DONE] 才算推进。心跳（`: ` 开头的注释行）、
// 空行、以及只带 role 或 finish_reason 的空帧一律不算。
//
// 为什么必须这么严：上游在思考或排队期间会持续发心跳与空占位帧。若把「收到
// 任意字节」当作存活证据，看门狗会被心跳无限续命、永不触发——实测表现就是
// 客户端一直挂着，直到上游自身的空闲阈值（可达 5 分钟）才断开。
func frameProgress(line string) bool {
	line = strings.TrimRight(line, "\r\n")
	if line == "" || strings.HasPrefix(line, ":") {
		return false // 心跳 / 注释
	}
	if !strings.HasPrefix(line, "data: ") {
		return false
	}
	payload := strings.TrimPrefix(line, "data: ")
	if strings.TrimSpace(payload) == "[DONE]" {
		return true
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []any  `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return false
	}
	for _, c := range chunk.Choices {
		if c.Delta.Content != "" || c.Delta.ReasoningContent != "" || len(c.Delta.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// bufferHasProgress 报告缓冲里是否已出现真实内容（用于首包闸门）。
func bufferHasProgress(buf []byte) bool {
	for _, line := range strings.Split(string(buf), "\n") {
		if frameProgress(line) {
			return true
		}
	}
	return false
}

type chunkMsg struct {
	data []byte
	err  error
}

// watchdogStream 把上游流包成「单读者」模型，并按内容推进情况设两道闸门。
//
// 唯一读取 goroutine 独占 body、按块投递给消费者；看门狗 goroutine 只在
// 「连续超时没有真实内容推进」时关闭上游连接并投递 ErrStreamStalled。
//
// 单读者的必要性：早期实现让探测 goroutine 与消费者共读同一 body，探测方
// 返回后仍继续读取并丢弃数据，会吞掉 tool_calls 的 id 首帧。
//
// 两道闸门的必要性：只有「首包」一道时，上游先发一帧真实内容再挂死就能绕过；
// 只有「任意字节」判据时，心跳会无限续命。
type watchdogStream struct {
	body    io.ReadCloser
	ch      chan chunkMsg
	done    chan struct{}
	closeMu sync.Once

	// firstTimeout 限制「首个真实内容」的等待时长。
	firstTimeout time.Duration
	// idleTimeout 限制「已有内容之后」的连续无内容时长。
	idleTimeout time.Duration

	// lastProgress 上次出现真实内容的时刻（不是「上次收到字节」）。
	lastProgress atomic.Int64
	sawContent   atomic.Bool
	stalled      atomic.Bool

	// scan 是跨块的行扫描残留，仅用于判定内容推进，不参与透传。
	scan []byte

	cur []byte
	err error
}

// newWatchdogStream 启动单读者包装。ctx 取消时同样会关闭上游连接。
func newWatchdogStream(ctx context.Context, rc io.ReadCloser, firstTimeout, idleTimeout time.Duration) *watchdogStream {
	ws := &watchdogStream{
		body:         rc,
		ch:           make(chan chunkMsg, 8),
		done:         make(chan struct{}),
		firstTimeout: firstTimeout,
		idleTimeout:  idleTimeout,
	}
	ws.lastProgress.Store(time.Now().UnixNano())

	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				ws.Close()
			case <-ws.done:
			}
		}()
	}

	if firstTimeout > 0 || idleTimeout > 0 {
		limit := firstTimeout
		if idleTimeout > 0 && (limit <= 0 || idleTimeout < limit) {
			limit = idleTimeout
		}
		tick := limit / 4
		if tick < 50*time.Millisecond {
			tick = 50 * time.Millisecond
		}
		go func() {
			t := time.NewTicker(tick)
			defer t.Stop()
			for {
				select {
				case <-ws.done:
					return
				case <-t.C:
					if time.Since(time.Unix(0, ws.lastProgress.Load())) <= ws.effectiveLimit() {
						continue
					}
					ws.stalled.Store(true)
					_ = ws.body.Close()
					return
				}
			}
		}()
	}

	go func() {
		defer close(ws.ch)
		buf := make([]byte, 32*1024)
		for {
			n, err := rc.Read(buf)
			if n > 0 {
				out := make([]byte, n)
				copy(out, buf[:n])
				ws.noteProgress(out)
				select {
				case ws.ch <- chunkMsg{data: out}:
				case <-ws.done:
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					err = nil
				}
				if ws.stalled.Load() {
					err = ErrStreamStalled
				}
				select {
				case ws.ch <- chunkMsg{err: err}:
				case <-ws.done:
				}
				return
			}
		}
	}()

	return ws
}

// effectiveLimit 返回当前适用的无内容上限：未见到内容用首包阈值，
// 见到内容后放宽到中途阈值。
func (s *watchdogStream) effectiveLimit() time.Duration {
	if s.sawContent.Load() {
		if s.idleTimeout > 0 {
			return s.idleTimeout
		}
		return s.firstTimeout
	}
	return s.firstTimeout
}

// noteProgress 按行扫描新增数据，仅在出现真实内容时推进计时。
func (s *watchdogStream) noteProgress(data []byte) {
	s.scan = append(s.scan, data...)
	for {
		i := bytes.IndexByte(s.scan, '\n')
		if i < 0 {
			break
		}
		line := string(s.scan[:i])
		s.scan = s.scan[i+1:]
		if frameProgress(line) {
			s.lastProgress.Store(time.Now().UnixNano())
			s.sawContent.Store(true)
		}
	}
	if len(s.scan) == 0 {
		s.scan = nil
	} else if len(s.scan) > 1<<16 {
		// 异常超长行（非 SSE 约定）：放弃扫描，避免无界增长。
		s.scan = nil
	}
}

func (s *watchdogStream) Read(p []byte) (int, error) {
	for len(s.cur) == 0 {
		msg, ok := <-s.ch
		if !ok {
			if s.err != nil {
				return 0, s.err
			}
			return 0, io.EOF
		}
		if len(msg.data) > 0 {
			s.cur = msg.data
			if msg.err != nil {
				s.err = msg.err
			}
			break
		}
		if msg.err != nil {
			return 0, msg.err
		}
	}
	n := copy(p, s.cur)
	s.cur = s.cur[n:]
	return n, nil
}

func (s *watchdogStream) Close() error {
	s.closeMu.Do(func() {
		close(s.done)
		_ = s.body.Close()
	})
	return nil
}

// readUntilFirstContent 读取直到出现首个真实内容（正文/思考/工具调用）。
//
// 上游只发心跳与空占位帧时，看门狗会以 ErrStreamStalled 中断，调用方可在
// 尚未向客户端写出任何字节的情况下无损换号重试。
func readUntilFirstContent(ws *watchdogStream) ([]byte, error) {
	var buf bytes.Buffer
	tmp := make([]byte, 16*1024)
	for {
		n, err := ws.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			if bufferHasProgress(buf.Bytes()) {
				return buf.Bytes(), nil
			}
		}
		if err != nil {
			if bufferHasProgress(buf.Bytes()) {
				return buf.Bytes(), nil
			}
			if buf.Len() == 0 {
				if errors.Is(err, io.EOF) {
					return nil, ErrEmptyStream
				}
				return nil, err
			}
			return nil, err
		}
	}
}

// replayStream 把已缓冲的前缀与流剩余部分拼回一个可关闭的 Reader。
type replayStream struct {
	io.Reader
	io.Closer
}

func newReplayStream(prefix []byte, ws *watchdogStream) *replayStream {
	return &replayStream{
		Reader: io.MultiReader(bytes.NewReader(prefix), ws),
		Closer: ws,
	}
}

// isStallError 判断是否属于上游假死或可静默重试的网络故障。
func isStallError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrStreamStalled) || errors.Is(err, ErrEmptyStream) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "timeout awaiting response headers") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "use of closed network connection")
}

// isRetryableStatus 判断上游 HTTP 状态码是否值得静默重试。
func isRetryableStatus(code int) bool {
	return code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout ||
		code == http.StatusInternalServerError
}
