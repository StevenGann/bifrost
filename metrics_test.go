package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsPeak(t *testing.T) {
	// Mon 2026-09-07 (verified Monday).
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"mon 02:00 peak", time.Date(2026, 9, 7, 2, 0, 0, 0, time.UTC), true},
		{"mon 03:59 peak", time.Date(2026, 9, 7, 3, 59, 0, 0, time.UTC), true},
		{"mon 05:00 off", time.Date(2026, 9, 7, 5, 0, 0, 0, time.UTC), false},
		{"mon 07:00 peak", time.Date(2026, 9, 7, 7, 0, 0, 0, time.UTC), true},
		{"mon 11:00 off", time.Date(2026, 9, 7, 11, 0, 0, 0, time.UTC), false},
		{"sat 02:00 off", time.Date(2026, 9, 5, 2, 0, 0, 0, time.UTC), false},
		{"sun 08:00 off", time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC), false},
	}
	for _, c := range cases {
		if got := isPeak(c.t); got != c.want {
			t.Errorf("%s: isPeak = %v, want %v (weekday %v)", c.name, got, c.want, c.t.Weekday())
		}
	}
}

func TestCostAt(t *testing.T) {
	m := newMetrics(defaultPricing())
	offPeak := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC) // Mon off-peak
	peak := time.Date(2026, 9, 7, 2, 0, 0, 0, time.UTC)     // Mon peak

	// flash off-peak: 1M in + 1M out = 0.22 + 0.66 = 0.88
	if got := m.costAt("deepseek-v4-flash", 1_000_000, 1_000_000, offPeak); got != 0.88 {
		t.Errorf("flash off-peak cost = %v, want 0.88", got)
	}
	// flash peak: doubled = 1.76
	if got := m.costAt("deepseek-v4-flash", 1_000_000, 1_000_000, peak); got != 1.76 {
		t.Errorf("flash peak cost = %v, want 1.76", got)
	}
	// unknown model = $0
	if got := m.costAt("llama3", 1_000_000, 1_000_000, offPeak); got != 0 {
		t.Errorf("unknown model cost = %v, want 0", got)
	}
}

func TestMetricsRender(t *testing.T) {
	m := newMetrics(defaultPricing())
	m.Record("coach", "deepseek-v4-flash", "patzer", "/api/chat", 200, 100, 50, 500*time.Millisecond, false)
	m.Record("coach", "deepseek-v4-flash", "patzer", "/api/chat", 200, 200, 100, 2*time.Second, false)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`bifrost_requests_total{model="coach",app="patzer",endpoint="/api/chat",status="200"} 2`,
		`bifrost_tokens_total{model="coach",app="patzer",direction="input"} 300`,
		`bifrost_tokens_total{model="coach",app="patzer",direction="output"} 150`,
		`bifrost_cost_usd_total{model="coach",app="patzer"} `,
		`bifrost_request_duration_seconds_count{model="coach",app="patzer"} 2`,
		`bifrost_request_duration_seconds_bucket{model="coach",app="patzer",le="1"} 1`,
		`bifrost_request_duration_seconds_bucket{model="coach",app="patzer",le="+Inf"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\nfull output:\n%s", want, body)
		}
	}
}

func TestChatCapturesUsage(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}},
			"usage": map[string]any{
				"prompt_tokens":         11,
				"completion_tokens":     7,
				"prompt_tokens_details": map[string]any{"cached_tokens": 3},
			},
		})
	}))
	defer mock.Close()

	b := Backend{Name: "default", BaseURL: mock.URL, APIKey: "k"}
	_, usage, err := chat(b, OpenAIRequest{Model: "m", Messages: []OpenAIMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if usage.PromptTokens != 11 || usage.CompletionTokens != 7 || usage.CachedTokens != 3 {
		t.Errorf("usage = %+v, want {11 7 3}", usage)
	}
}

func TestStreamChatCapturesUsage(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":13,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":4}}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer mock.Close()

	b := Backend{Name: "default", BaseURL: mock.URL, APIKey: "k"}
	var usage Usage
	_, err := streamChat(b, OpenAIRequest{Model: "m", Messages: []OpenAIMessage{{Role: "user", Content: "hi"}}}, func(string) error { return nil }, &usage)
	if err != nil {
		t.Fatalf("streamChat: %v", err)
	}
	if usage.PromptTokens != 13 || usage.CompletionTokens != 2 || usage.CachedTokens != 4 {
		t.Errorf("usage = %+v, want {13 2 4}", usage)
	}
}
