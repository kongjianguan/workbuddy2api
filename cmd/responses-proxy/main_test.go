package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
		upstream: u,
		client:   &http.Client{Timeout: 5 * time.Second},
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

func TestResponsesProxyConvertsSync(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	s := &server{upstream: u, client: &http.Client{Timeout: 5 * time.Second}}

	body := `{"model":"m","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.responses(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["object"] != "response" {
		t.Errorf("object=%v", out["object"])
	}
	if out["status"] != "completed" {
		t.Errorf("status=%v", out["status"])
	}
}

func TestResponsesProxyForwardsUpstreamError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	s := &server{upstream: u, client: &http.Client{Timeout: 5 * time.Second}}

	body := `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.responses(rec, req)
	if rec.Code != 401 {
		t.Fatalf("status=%d want 401 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "bad key") {
		t.Errorf("body=%s", rec.Body.String())
	}
}
