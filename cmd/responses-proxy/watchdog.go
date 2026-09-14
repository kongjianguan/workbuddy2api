package main

import (
	"bytes"
	"context"
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
	// ErrStreamStalled 表示 watchdog 超时内没有收到任何新数据（含首包假死与中途断流）。
	ErrStreamStalled = errors.New("upstream stream stalled: no data within watchdog timeout")
	// ErrEmptyStream 表示上游返回 200 后未发送任何数据即关闭。
	ErrEmptyStream = errors.New("upstream closed stream without sending data")
)

// hasActualContent 判断缓冲数据里是否已出现真实的正文 / 思考 / 工具调用。
// 仅含 role:assistant 的空占位包不算——上游常在 1 秒时先发这种包再挂死。
func hasActualContent(data []byte) bool {
	s := string(data)
	if strings.Contains(s, "[DONE]") {
		return true
	}
	return strings.Contains(s, `"content"`) ||
		strings.Contains(s, `"reasoning_content"`) ||
		strings.Contains(s, `"tool_calls"`)
}

type chunkMsg struct {
	data []byte
	err  error
}

// watchdogStream 把上游流包成「单读者」模型。
//
// 唯一的读取 goroutine 独占 body，按块投递给消费者；另有一个看门狗 goroutine
// 在连续 timeout 没有新数据时关闭上游连接，投递 ErrStreamStalled。
//
// 为什么必须是单读者：早期实现让 peeking goroutine 先 Read 一段、消费者再继续
// 从同一个 body 读。那个 goroutine 在探测成功后并未停止，会继续把后续数据块
// 抢进一个无人消费的 channel 并丢弃——工具调用首帧（带 id）被吞掉后，客户端
// 合并出的 tool_call 缺少 id，报 "Expected 'id' to be a string."。
type watchdogStream struct {
	body     io.ReadCloser
	ch       chan chunkMsg
	timeout  time.Duration
	cur      []byte
	done     chan struct{}
	closeOne sync.Once
	lastRead atomic.Int64
	// stalled 由看门狗在超时关闭连接前置位，用于把底层 IO 错误还原成 ErrStreamStalled。
	stalled atomic.Bool
	err     error
}

// newWatchdogStream 启动单读者包装。ctx 取消时同样会关闭上游连接。
func newWatchdogStream(ctx context.Context, rc io.ReadCloser, timeout time.Duration) *watchdogStream {
	ws := &watchdogStream{
		body:    rc,
		ch:      make(chan chunkMsg, 8),
		timeout: timeout,
		done:    make(chan struct{}),
	}
	ws.lastRead.Store(time.Now().UnixNano())

	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				ws.Close()
			case <-ws.done:
			}
		}()
	}

	if timeout > 0 {
		tick := timeout / 4
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
					if time.Since(time.Unix(0, ws.lastRead.Load())) > timeout {
						ws.stalled.Store(true)
						_ = ws.body.Close()
						return
					}
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
				ws.lastRead.Store(time.Now().UnixNano())
				out := make([]byte, n)
				copy(out, buf[:n])
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
	s.closeOne.Do(func() {
		close(s.done)
		_ = s.body.Close()
	})
	return nil
}

// readUntilActualContent 读取直到出现真实内容（正文/思考/工具调用）。
// 上游只发空占位包就挂死时返回 ErrStreamStalled，调用方可无损换号重试。
func readUntilActualContent(ws *watchdogStream) ([]byte, error) {
	var buf bytes.Buffer
	tmp := make([]byte, 16*1024)
	for {
		n, err := ws.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			if hasActualContent(buf.Bytes()) {
				return buf.Bytes(), nil
			}
		}
		if err != nil {
			if hasActualContent(buf.Bytes()) {
				return buf.Bytes(), nil
			}
			if len(buf.Bytes()) == 0 {
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
