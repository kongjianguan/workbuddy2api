// responses-proxy 把 OpenAI Responses API 转成 Chat Completions 再转发给上游网关。
//
// 只做协议转换，不碰账号池。Codex 指本进程，本进程把 /v1/responses 转成
// POST /v1/chat/completions 打到 -upstream（原作者 workbuddy2api 即可）。
//
// 同时内建首 Token 看门狗（Watchdog）与静默换号重试机制：当上游返回 200 OK 但
// 0 token 假死挂起时，在设定超时（默认 15s）内主动掐断并无缝重试，彻底消灭 154s 挂死黑洞。
// 此外支持请求全量/错误转储落盘（-dump-dir），便于精确排查上游 400 格式错误。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/responses"
)

func main() {
	listen := flag.String("listen", ":7865", "listen address")
	upstream := flag.String("upstream", "http://127.0.0.1:7863", "chat-completions gateway base URL")
	aliasFile := flag.String("aliases", "", "JSON object of client model → upstream model (optional)")
	watchdogTimeout := flag.Duration("watchdog-timeout", 15*time.Second, "max wait for first real content before retrying")
	idleTimeout := flag.Duration("stream-idle-timeout", 45*time.Second, "max gap between content frames once streaming has started")
	maxRetries := flag.Int("max-retries", 1, "maximum silent retry attempts on upstream stall or retryable errors")
	dumpDir := flag.String("dump-dir", "", "directory to dump raw requests and error payloads for debugging (optional)")
	flag.Parse()

	u, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("upstream URL: %v", err)
	}

	aliases, err := loadAliases(*aliasFile)
	if err != nil {
		log.Fatalf("aliases: %v", err)
	}

	dumper := NewRequestDumper(*dumpDir)

	s := &server{
		upstream:        u,
		aliases:         aliases,
		watchdogTimeout: *watchdogTimeout,
		idleTimeout:     *idleTimeout,
		maxRetries:      *maxRetries,
		dumper:          dumper,
		client: &http.Client{
			Timeout: 0, // 流式聊天无总时长上限
			Transport: &http.Transport{
				ResponseHeaderTimeout: 120 * time.Second,
			},
		},
		proxy: httputil.NewSingleHostReverseProxy(u),
	}
	orig := s.proxy.Director
	s.proxy.Director = func(r *http.Request) {
		orig(r)
		r.Host = u.Host
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", s.responses)
	mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("/", s.passthrough)

	log.Printf("responses-proxy listening on %s → %s (%d aliases, first-content=%v, stream-idle=%v, max_retries=%d, dump=%q)",
		*listen, u.String(), len(aliases), *watchdogTimeout, *idleTimeout, *maxRetries, *dumpDir)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatal(err)
	}
}

type server struct {
	upstream        *url.URL
	aliases         map[string]string
	watchdogTimeout time.Duration
	idleTimeout     time.Duration
	maxRetries      int
	dumper          *RequestDumper
	client          *http.Client
	proxy           *httputil.ReverseProxy
}

func loadAliases(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func applyModelAlias(body []byte, model *string, aliases map[string]string) []byte {
	if len(aliases) == 0 || model == nil || *model == "" {
		return body
	}
	target, ok := aliases[*model]
	if !ok || target == "" {
		return body
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	raw, err := json.Marshal(target)
	if err != nil {
		return body
	}
	obj["model"] = raw
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	log.Printf("model alias %s -> %s", *model, target)
	*model = target
	return out
}

func applyModelAliasChat(body []byte, aliases map[string]string) ([]byte, string, string) {
	if len(aliases) == 0 {
		return body, "", ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return body, "", ""
	}
	rawModel, ok := obj["model"]
	if !ok {
		return body, "", ""
	}
	var origModel string
	if json.Unmarshal(rawModel, &origModel) != nil || origModel == "" {
		return body, "", ""
	}
	target, ok := aliases[origModel]
	if !ok || target == "" {
		return body, origModel, origModel
	}
	raw, err := json.Marshal(target)
	if err != nil {
		return body, origModel, origModel
	}
	obj["model"] = raw
	out, err := json.Marshal(obj)
	if err != nil {
		return body, origModel, origModel
	}
	log.Printf("model alias %s -> %s", origModel, target)
	return out, origModel, target
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"service":"responses-proxy"}`))
}

func (s *server) passthrough(w http.ResponseWriter, r *http.Request) {
	s.proxy.ServeHTTP(w, r)
}

func (s *server) responses(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := s.dumper.NextID()

	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	req, chatBody, toolCtx, ok := responses.Convert(w, body, 8<<20)
	if !ok {
		s.recordDump(reqID, start, "/v1/responses", "", "", body, nil, http.StatusBadRequest, "convert_request_failed")
		return
	}
	origModel := req.Model
	chatBody = applyModelAlias(chatBody, &req.Model, s.aliases)

	// 转换后的 Chat 体同样可能带着历史里不成对的工具调用 id。
	if fixed, changed := sanitizeToolCallIDs(chatBody); changed {
		chatBody = fixed
		log.Printf("[sanitize] repaired unpaired tool_call ids")
	}

	upURL := s.upstream.ResolveReference(&url.URL{Path: "/v1/chat/completions"})
	maxAttempts := s.maxRetries + 1

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			log.Printf("[watchdog] retrying /v1/responses (attempt %d/%d)...", attempt, maxAttempts)
		}

		upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upURL.String(), bytes.NewReader(chatBody))
		if err != nil {
			s.recordDump(reqID, start, "/v1/responses", origModel, req.Model, body, chatBody, http.StatusBadGateway, err.Error())
			writeProxyError(w, http.StatusBadGateway, err.Error())
			return
		}
		upReq.Header.Set("Content-Type", "application/json")
		if authz := r.Header.Get("Authorization"); authz != "" {
			upReq.Header.Set("Authorization", authz)
		}

		resp, err := s.client.Do(upReq)
		if err != nil {
			if isStallError(err) && attempt < maxAttempts {
				log.Printf("[watchdog] upstream request error: %v, will retry", err)
				continue
			}
			s.recordDump(reqID, start, "/v1/responses", origModel, req.Model, body, chatBody, http.StatusBadGateway, err.Error())
			writeProxyError(w, http.StatusBadGateway, "upstream: "+err.Error())
			return
		}

		// 如果遇到可重试状态码（如 502/503/504），尝试换号重试
		if isRetryableStatus(resp.StatusCode) && attempt < maxAttempts {
			log.Printf("[watchdog] upstream returned status %d, will retry", resp.StatusCode)
			_ = resp.Body.Close()
			continue
		}

		if resp.StatusCode >= 400 {
			defer resp.Body.Close()
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			s.recordDump(reqID, start, "/v1/responses", origModel, req.Model, body, chatBody, resp.StatusCode, string(errBody))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(errBody)
			return
		}

		ct := resp.Header.Get("Content-Type")
		isStream := req.Stream || strings.Contains(ct, "text/event-stream")

		if isStream {
			// 单读者看门狗：唯一的读取 goroutine 独占上游 body，同时负责
			// 首包探测与中途空闲检测。绝不能出现第二个读者——否则它会抢走
			// tool_calls 首帧（带 id 的那帧）导致客户端解析报错。
			ws := newWatchdogStream(r.Context(), resp.Body, s.watchdogTimeout, s.idleTimeout)
			prefix, err := readUntilFirstContent(ws)
			if err != nil {
				_ = ws.Close()
				if isStallError(err) && attempt < maxAttempts {
					log.Printf("[watchdog] upstream stalled without content: %v, will retry", err)
					continue
				}
				s.recordDump(reqID, start, "/v1/responses", origModel, req.Model, body, chatBody, http.StatusGatewayTimeout, err.Error())
				writeProxyError(w, http.StatusGatewayTimeout, fmt.Sprintf("upstream stalled: %v", err))
				return
			}

			tr := responses.NewStreamTranslator(w, toolCtx, req.Model)
			if err := tr.Run(newReplayStream(prefix, ws)); err != nil {
				log.Printf("stream translate: %v", err)
				s.recordDump(reqID, start, "/v1/responses", origModel, req.Model, body, chatBody, http.StatusOK, "stream_err: "+err.Error())
			} else {
				s.recordDump(reqID, start, "/v1/responses", origModel, req.Model, body, chatBody, http.StatusOK, "")
			}
			_ = ws.Close()
			return
		}

		// 非流式响应处理
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if err != nil {
			if isStallError(err) && attempt < maxAttempts {
				log.Printf("[watchdog] read sync body failed: %v, will retry", err)
				continue
			}
			s.recordDump(reqID, start, "/v1/responses", origModel, req.Model, body, chatBody, http.StatusBadGateway, err.Error())
			writeProxyError(w, http.StatusBadGateway, err.Error())
			return
		}

		var chatResp map[string]any
		if err := json.Unmarshal(raw, &chatResp); err != nil {
			s.recordDump(reqID, start, "/v1/responses", origModel, req.Model, body, chatBody, http.StatusBadGateway, "upstream JSON: "+err.Error())
			writeProxyError(w, http.StatusBadGateway, "upstream JSON: "+err.Error())
			return
		}
		out := responses.BuildResponse(chatResp, toolCtx, req.Model)
		s.recordDump(reqID, start, "/v1/responses", origModel, req.Model, body, chatBody, http.StatusOK, "")
		responses.WriteJSON(w, http.StatusOK, out)
		return
	}

	s.recordDump(reqID, start, "/v1/responses", origModel, req.Model, body, chatBody, http.StatusGatewayTimeout, "upstream stalled after retries")
	writeProxyError(w, http.StatusGatewayTimeout, "upstream stalled with 0 tokens after retries")
}

func (s *server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := s.dumper.NextID()

	rawClientBody, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 修复历史里不成对的工具调用 id（上游对空串/不配对一律 400 model_param_invalid）。
	clientBody := rawClientBody
	if fixed, changed := sanitizeToolCallIDs(rawClientBody); changed {
		clientBody = fixed
		log.Printf("[sanitize] repaired unpaired tool_call ids")
	}

	body, origModel, targetModel := applyModelAliasChat(clientBody, s.aliases)
	upURL := s.upstream.ResolveReference(&url.URL{Path: "/v1/chat/completions"})
	maxAttempts := s.maxRetries + 1

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			log.Printf("[watchdog] retrying /v1/chat/completions (attempt %d/%d)...", attempt, maxAttempts)
		}

		upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upURL.String(), bytes.NewReader(body))
		if err != nil {
			s.recordDump(reqID, start, "/v1/chat/completions", origModel, targetModel, rawClientBody, body, http.StatusBadGateway, err.Error())
			writeProxyError(w, http.StatusBadGateway, err.Error())
			return
		}
		upReq.Header.Set("Content-Type", "application/json")
		if authz := r.Header.Get("Authorization"); authz != "" {
			upReq.Header.Set("Authorization", authz)
		}

		resp, err := s.client.Do(upReq)
		if err != nil {
			if isStallError(err) && attempt < maxAttempts {
				log.Printf("[watchdog] upstream request error: %v, will retry", err)
				continue
			}
			s.recordDump(reqID, start, "/v1/chat/completions", origModel, targetModel, rawClientBody, body, http.StatusBadGateway, err.Error())
			writeProxyError(w, http.StatusBadGateway, "upstream: "+err.Error())
			return
		}

		if isRetryableStatus(resp.StatusCode) && attempt < maxAttempts {
			log.Printf("[watchdog] upstream returned status %d, will retry", resp.StatusCode)
			_ = resp.Body.Close()
			continue
		}

		if resp.StatusCode >= 400 {
			defer resp.Body.Close()
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			s.recordDump(reqID, start, "/v1/chat/completions", origModel, targetModel, rawClientBody, body, resp.StatusCode, string(errBody))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(errBody)
			return
		}

		ct := resp.Header.Get("Content-Type")
		isStream := strings.Contains(ct, "text/event-stream")

		if isStream {
			// 单读者看门狗：见 responses 分支的同名注释——第二个读者会抢走
			// tool_calls 首帧（带 id），导致客户端报 "Expected 'id' to be a string."
			ws := newWatchdogStream(r.Context(), resp.Body, s.watchdogTimeout, s.idleTimeout)
			prefix, err := readUntilFirstContent(ws)
			if err != nil {
				_ = ws.Close()
				if isStallError(err) && attempt < maxAttempts {
					log.Printf("[watchdog] upstream stalled without content: %v, will retry", err)
					continue
				}
				s.recordDump(reqID, start, "/v1/chat/completions", origModel, targetModel, rawClientBody, body, http.StatusGatewayTimeout, err.Error())
				writeProxyError(w, http.StatusGatewayTimeout, fmt.Sprintf("upstream stalled: %v", err))
				return
			}

			for k, vv := range resp.Header {
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)

			flusher, ok := w.(http.Flusher)
			if len(prefix) > 0 {
				_, _ = w.Write(prefix)
				if ok {
					flusher.Flush()
				}
			}
			buf := make([]byte, 4096)
			for {
				n, rerr := ws.Read(buf)
				if n > 0 {
					_, _ = w.Write(buf[:n])
					if ok {
						flusher.Flush()
					}
				}
				if rerr != nil {
					if rerr != io.EOF {
						log.Printf("[watchdog] chat stream: %v", rerr)
					}
					// 上游中途断流时，光断开连接会让客户端把「截断」读成
					// 「协议异常」——DSH 的 parseSse 缺 [DONE] 直接抛
					// STREAM_CLOSED，用户看到的是报错而不是可重试的失败。
					// 补一个终止帧把结果收敛成明确的结束语义。
					// 此类流已写过响应头，无法再改状态码，补帧是唯一的收尾手段。
					if frame := ws.terminationFrame(); frame != "" {
						_, _ = w.Write([]byte(frame))
						if ok {
							flusher.Flush()
						}
					}
					break
				}
			}
			_ = ws.Close()
			s.recordDump(reqID, start, "/v1/chat/completions", origModel, targetModel, rawClientBody, body, http.StatusOK, "")
			return
		}

		// 非流式响应直接复制
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		_ = resp.Body.Close()
		s.recordDump(reqID, start, "/v1/chat/completions", origModel, targetModel, rawClientBody, body, http.StatusOK, "")
		return
	}

	s.recordDump(reqID, start, "/v1/chat/completions", origModel, targetModel, rawClientBody, body, http.StatusGatewayTimeout, "upstream stalled after retries")
	writeProxyError(w, http.StatusGatewayTimeout, "upstream stalled with 0 tokens after retries")
}

func (s *server) recordDump(id string, start time.Time, endpoint, clientModel, upModel string, rawClient, convertedChat []byte, status int, errMsg string) {
	if s.dumper == nil {
		return
	}
	s.dumper.Record(DumpRecord{
		ID:                id,
		Timestamp:         start.Format(time.RFC3339),
		Endpoint:          endpoint,
		ClientModel:       clientModel,
		UpstreamModel:     upModel,
		RawClientBody:     rawClient,
		ConvertedChatBody: convertedChat,
		StatusCode:        status,
		Error:             errMsg,
		DurationMs:        time.Since(start).Milliseconds(),
	})
}

func writeProxyError(w http.ResponseWriter, status int, msg string) {
	responses.WriteJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    "upstream_error",
		},
	})
}

func copyUpstreamError(w http.ResponseWriter, resp *http.Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func init() {
	if os.Getenv("RESPONSES_PROXY_QUIET") == "1" {
		log.SetOutput(io.Discard)
	}
}
