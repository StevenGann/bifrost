package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCacheSetGet(t *testing.T) {
	c := newCache(10, time.Minute)
	c.Set("k", []byte("v"))
	if v, ok := c.Get("k"); !ok || string(v) != "v" {
		t.Fatalf("got %q %v, want v true", v, ok)
	}
	if _, ok := c.Get("missing"); ok {
		t.Fatal("expected miss")
	}
}

func TestCacheExpiry(t *testing.T) {
	c := newCache(10, time.Millisecond)
	c.Set("k", []byte("v"))
	time.Sleep(5 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("expected expiry")
	}
}

func TestCacheEviction(t *testing.T) {
	c := newCache(2, time.Minute)
	c.Set("a", []byte("1"))
	c.Set("b", []byte("2"))
	c.Set("c", []byte("3")) // evicts a (least recently used)
	if _, ok := c.Get("a"); ok {
		t.Fatal("a should be evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b should remain")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c should remain")
	}
}

func TestCacheKeyStable(t *testing.T) {
	k1 := cacheKey("/api/chat", ollamaChatRequest{Model: "coach"})
	k2 := cacheKey("/api/chat", ollamaChatRequest{Model: "coach"})
	if k1 != k2 || k1 == "" {
		t.Fatalf("key not stable: %q vs %q", k1, k2)
	}
	if k3 := cacheKey("/api/generate", ollamaChatRequest{Model: "coach"}); k1 == k3 {
		t.Fatal("different endpoints must yield different keys")
	}
}

func TestCompleteCachedHit(t *testing.T) {
	oldCache, oldMetrics := lruCache, metrics
	lruCache = newCache(100, time.Minute)
	metrics = newMetrics(defaultPricing())
	defer func() { lruCache, metrics = oldCache, oldMetrics }()

	var calls int
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "cached-ok"}}}})
	}))
	defer mock.Close()

	cfg := Config{
		Backends: map[string]Backend{"b": {Name: "b", BaseURL: mock.URL, Local: false}},
		Default:  "b",
		Routes:   map[string]Route{"coach": {Name: "coach", Backend: "b", Upstream: "deepseek-v4-flash"}},
	}

	body := `{"model":"coach","messages":[{"role":"user","content":"hi"}],"stream":false}`
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handleChat(rec, req, cfg)
		if rec.Code != 200 {
			t.Fatalf("iter %d status %d body %s", i, rec.Code, rec.Body.String())
		}
		var resp ollamaChatResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		if resp.Message.Content != "cached-ok" {
			t.Fatalf("content %q", resp.Message.Content)
		}
	}

	if calls != 1 {
		t.Errorf("expected 1 upstream call (2nd served from cache), got %d", calls)
	}
	if got := metrics.cacheHits["coach|unknown"]; got != 1 {
		t.Errorf("cache hits = %d, want 1", got)
	}
	if got := metrics.cacheMisses["coach|unknown"]; got != 1 {
		t.Errorf("cache misses = %d, want 1", got)
	}
}
