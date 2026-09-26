package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbedCallsBackend(t *testing.T) {
	var gotPath string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float64{0.1, 0.2, 0.3}, "index": 0, "object": "embedding"}}})
	}))
	defer mock.Close()

	b := Backend{Name: "mock", BaseURL: mock.URL, Local: true}
	emb, err := embed(b, "nomic-embed-text", "hello")
	if err != nil {
		t.Fatalf("embed err: %v", err)
	}
	if len(emb) != 3 || emb[0] != 0.1 {
		t.Fatalf("unexpected embedding: %v", emb)
	}
	if gotPath != "/v1/embeddings" {
		t.Fatalf("path = %q, want /v1/embeddings", gotPath)
	}
}

func TestHandleEmbeddings(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float64{1.0, 2.0}, "index": 0}}})
	}))
	defer mock.Close()

	cfg := Config{
		Backends: map[string]Backend{"mock": {Name: "mock", BaseURL: mock.URL, Local: true}},
		Default:  "mock",
		Routes:   map[string]Route{"embed": {Name: "embed", Backend: "mock", Upstream: "nomic-embed-text"}},
	}

	req := httptest.NewRequest("POST", "/api/embeddings", strings.NewReader(`{"model":"embed","prompt":"hello"}`))
	rec := httptest.NewRecorder()
	handleEmbeddings(rec, req, cfg)

	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var resp ollamaEmbeddingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(resp.Embedding) != 2 {
		t.Fatalf("embedding len %d, want 2", len(resp.Embedding))
	}
}

func TestChatUsesV1Path(t *testing.T) {
	var gotPath string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}}})
	}))
	defer mock.Close()

	cfg := Config{
		Backends: map[string]Backend{"b": {Name: "b", BaseURL: mock.URL, Local: false}},
		Default:  "b",
		Routes:   map[string]Route{"coach": {Name: "coach", Backend: "b", Upstream: "deepseek-v4-flash"}},
	}

	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"model":"coach","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	rec := httptest.NewRecorder()
	handleChat(rec, req, cfg)

	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", gotPath)
	}
}
