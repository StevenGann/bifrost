package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCosine(t *testing.T) {
	cases := []struct {
		a, b []float64
		want float64
	}{
		{[]float64{1, 0, 0}, []float64{1, 0, 0}, 1.0},
		{[]float64{1, 0}, []float64{0, 1}, 0.0},
		{[]float64{1}, []float64{2}, 1.0}, // same direction, different magnitude
		{[]float64{1, 0}, []float64{1, 1}, 0.7071067811865475},
		{[]float64{}, []float64{1}, 0.0},
		{[]float64{1, 0}, []float64{1, 0, 0}, 0.0}, // length mismatch
	}
	for _, c := range cases {
		if got := cosine(c.a, c.b); got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("cosine(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestQueryText(t *testing.T) {
	req := OpenAIRequest{Messages: []OpenAIMessage{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "hello"},
	}}
	if got, want := queryText(req), "system: be brief\nuser: hello"; got != want {
		t.Errorf("queryText = %q, want %q", got, want)
	}
	if queryText(OpenAIRequest{}) != "" {
		t.Error("empty request should yield empty text")
	}
}

func TestSemanticCacheGetSet(t *testing.T) {
	sc := newSemanticCache(10, time.Minute, 0.9)
	sc.set("m", []float64{1, 0}, []byte("a"))
	if v, ok := sc.get("m", []float64{1, 0}); !ok || string(v) != "a" {
		t.Errorf("expected hit, got %q %v", v, ok)
	}
	if _, ok := sc.get("m", []float64{0, 1}); ok {
		t.Error("expected miss for orthogonal vector")
	}
	if _, ok := sc.get("other", []float64{1, 0}); ok {
		t.Error("expected miss for a different model")
	}
}

func TestSemanticCacheThreshold(t *testing.T) {
	sc := newSemanticCache(10, time.Minute, 0.95)
	sc.set("m", []float64{1, 0}, []byte("a"))
	// [1,0] vs [1,1] → cosine 0.707 < 0.95 → miss
	if _, ok := sc.get("m", []float64{1, 1}); ok {
		t.Error("expected miss below threshold")
	}
}

func TestSemanticCacheEviction(t *testing.T) {
	sc := newSemanticCache(2, time.Minute, 0.5)
	sc.set("m", []float64{1, 0, 0}, []byte("a"))
	sc.set("m", []float64{0, 1, 0}, []byte("b"))
	sc.set("m", []float64{0, 0, 1}, []byte("c"))
	if _, ok := sc.get("m", []float64{1, 0, 0}); ok {
		t.Error("expected oldest entry 'a' to be evicted")
	}
	if v, ok := sc.get("m", []float64{0, 0, 1}); !ok || string(v) != "c" {
		t.Errorf("expected 'c' present, got %q %v", v, ok)
	}
}

func TestCompleteCachedSemanticHit(t *testing.T) {
	old := semanticCache
	semanticCache = newSemanticCache(10, time.Minute, 0.9)
	defer func() { semanticCache = old }()

	var chatCalls int32
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float64{1, 0, 0}, "index": 0, "object": "embedding"}},
		})
	}))
	defer embedSrv.Close()

	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&chatCalls, 1)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "Paris"}}},
		})
	}))
	defer chatSrv.Close()

	cfg := Config{
		Backends: map[string]Backend{
			"embed": {Name: "embed", BaseURL: embedSrv.URL},
			"chat":  {Name: "chat", BaseURL: chatSrv.URL},
		},
		Routes: map[string]Route{
			"embed": {Name: "embed", Backend: "embed", Upstream: "emb-model"},
			"coach": {Name: "coach", Backend: "chat", Upstream: "chat-model"},
		},
		EmbedModel: "embed",
	}
	build := func(msg string) func(string) OpenAIRequest {
		return func(up string) OpenAIRequest {
			return OpenAIRequest{Model: up, Messages: []OpenAIMessage{{Role: "user", Content: msg}}}
		}
	}

	// First request: semantic miss → backend call → stored.
	r1, _, status, err := cfg.completeCached("key1", "coach", "app", "/api/chat", build("What is the capital of France?"))
	if err != nil || status != 200 {
		t.Fatalf("first request: status=%d err=%v", status, err)
	}
	if got := r1.Choices[0].Message.Content; got != "Paris" {
		t.Fatalf("first = %q", got)
	}

	// Second request, different wording + different key → exact miss, semantic hit.
	r2, _, status2, err2 := cfg.completeCached("key2", "coach", "app", "/api/chat", build("What's the capital of France?"))
	if err2 != nil || status2 != 200 {
		t.Fatalf("second request: status=%d err=%v", status2, err2)
	}
	if got := r2.Choices[0].Message.Content; got != "Paris" {
		t.Fatalf("second = %q", got)
	}
	if atomic.LoadInt32(&chatCalls) != 1 {
		t.Fatalf("expected 1 backend chat call (semantic hit should skip the backend), got %d", chatCalls)
	}
}
