// responses-proxy 把 OpenAI Responses API 转成 Chat Completions 再转发给上游网关。
//
// 只做协议转换，不碰账号池。Codex 指本进程，本进程把 /v1/responses 转成
// POST /v1/chat/completions 打到 -upstream（原作者 workbuddy2api 即可）。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
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
	flag.Parse()

	u, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("upstream URL: %v", err)
	}

	s := &server{
		upstream: u,
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
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("/", s.passthrough)

	log.Printf("responses-proxy listening on %s → %s", *listen, u.String())
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatal(err)
	}
}

type server struct {
	upstream *url.URL
	client   *http.Client
	proxy    *httputil.ReverseProxy
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"service":"responses-proxy"}`))
}

func (s *server) passthrough(w http.ResponseWriter, r *http.Request) {
	s.proxy.ServeHTTP(w, r)
}

func (s *server) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	req, chatBody, toolCtx, ok := responses.Convert(w, body, 8<<20)
	if !ok {
		return
	}

	upURL := s.upstream.ResolveReference(&url.URL{Path: "/v1/chat/completions"})
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upURL.String(), bytes.NewReader(chatBody))
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, err.Error())
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	if authz := r.Header.Get("Authorization"); authz != "" {
		upReq.Header.Set("Authorization", authz)
	}

	resp, err := s.client.Do(upReq)
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, "upstream: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		copyUpstreamError(w, resp)
		return
	}

	ct := resp.Header.Get("Content-Type")
	if req.Stream || strings.Contains(ct, "text/event-stream") {
		tr := responses.NewStreamTranslator(w, toolCtx, req.Model)
		if err := tr.Run(resp.Body); err != nil {
			log.Printf("stream translate: %v", err)
		}
		return
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, err.Error())
		return
	}
	var chatResp map[string]any
	if err := json.Unmarshal(raw, &chatResp); err != nil {
		writeProxyError(w, http.StatusBadGateway, "upstream JSON: "+err.Error())
		return
	}
	out := responses.BuildResponse(chatResp, toolCtx, req.Model)
	responses.WriteJSON(w, http.StatusOK, out)
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
