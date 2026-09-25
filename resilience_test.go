package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRetryOnTransient(t *testing.T) {
	var calls int
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}}})
	}))
	defer mock.Close()

	cfg := testConfig(mock.URL)
	cfg.Retries = 1
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"model":"coach","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	rec := httptest.NewRecorder()
	handleChat(rec, req, cfg)

	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if calls != 2 {
		t.Errorf("expected 2 upstream calls (1 retry), got %d", calls)
	}
}

func TestNoRetryOnPermanent(t *testing.T) {
	var calls int
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer mock.Close()

	cfg := testConfig(mock.URL)
	cfg.Retries = 2
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"model":"coach","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	rec := httptest.NewRecorder()
	handleChat(rec, req, cfg)

	if rec.Code != 400 {
		t.Errorf("expected 400, got %d", rec.Code)
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on 400), got %d", calls)
	}
}

func TestFallback(t *testing.T) {
	metrics = newMetrics(defaultPricing())
	var primaryCalls, fallbackCalls int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "fallback-ok"}}}})
	}))
	defer fallback.Close()

	cfg := Config{
		Port: "11434",
		Backends: map[string]Backend{
			"primary":  {Name: "primary", BaseURL: primary.URL, Local: false},
			"fallback": {Name: "fallback", BaseURL: fallback.URL, Local: false},
		},
		Default: "primary",
		Routes: map[string]Route{
			"coach":          {Name: "coach", Backend: "primary", Upstream: "deepseek-v4-flash"},
			"fallback-model": {Name: "fallback-model", Backend: "fallback", Upstream: "deepseek-v4-flash"},
		},
		Retries:   0,
		Fallbacks: map[string]string{"coach": "fallback-model"},
	}
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"model":"coach","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	rec := httptest.NewRecorder()
	handleChat(rec, req, cfg)

	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var resp ollamaChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp.Message.Content != "fallback-ok" {
		t.Errorf("expected fallback content, got %q", resp.Message.Content)
	}
	if primaryCalls != 1 || fallbackCalls != 1 {
		t.Errorf("expected 1 primary + 1 fallback call, got %d + %d", primaryCalls, fallbackCalls)
	}
	if got := metrics.fallbacks["coach|unknown"]; got != 1 {
		t.Errorf("fallback metric = %d, want 1", got)
	}
}
