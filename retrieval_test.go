package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestChunkText(t *testing.T) {
	if got, want := chunkText("abcdefghij", 4, 0), []string{"abcd", "efgh", "ij"}; !reflect.DeepEqual(got, want) {
		t.Errorf("chunkText = %q, want %q", got, want)
	}
	if got, want := chunkText("abcdef", 4, 2), []string{"abcd", "cdef"}; !reflect.DeepEqual(got, want) {
		t.Errorf("chunkText overlap = %q, want %q", got, want)
	}
	if got := chunkText("   ", 4, 0); got != nil {
		t.Errorf("chunkText blank = %q, want nil", got)
	}
}

func TestLastUserContent(t *testing.T) {
	msgs := []OpenAIMessage{
		{Role: "user", Content: "a"},
		{Role: "assistant", Content: "b"},
		{Role: "user", Content: "c"},
	}
	if got := lastUserContent(msgs); got != "c" {
		t.Errorf("lastUserContent = %q, want %q", got, "c")
	}
	if got := lastUserContent([]OpenAIMessage{{Role: "assistant", Content: "x"}}); got != "" {
		t.Errorf("no user message should yield empty, got %q", got)
	}
}

func TestVectorIndexSearch(t *testing.T) {
	v := newVectorIndex(0)
	v.Add(chunk{Text: "a", Embedding: []float64{1, 0}}, chunk{Text: "b", Embedding: []float64{0, 1}})
	hits := v.Search([]float64{1, 0}, 2)
	if len(hits) != 2 || hits[0].Text != "a" || hits[1].Text != "b" {
		t.Fatalf("search order wrong: %+v", hits)
	}
	if hits[0].Score != 1.0 {
		t.Errorf("top score = %v, want 1.0", hits[0].Score)
	}
	if top := v.Search([]float64{1, 0}, 1); len(top) != 1 || top[0].Text != "a" {
		t.Errorf("top-1 = %+v", top)
	}
}

func TestContextHelpers(t *testing.T) {
	if out := prependContext([]OpenAIMessage{{Role: "user", Content: "hi"}}, "ctx text"); len(out) != 2 ||
		out[0].Role != "system" || !strings.Contains(out[0].Content, "ctx text") {
		t.Errorf("prependContext = %+v", out)
	}
	if out := contextSystem("orig system", "ctx"); !strings.Contains(out, "ctx") || !strings.Contains(out, "orig system") {
		t.Errorf("contextSystem = %q", out)
	}
}

func TestIngestAndRetrieve(t *testing.T) {
	old := docIndex
	docIndex = newVectorIndex(0)
	defer func() { docIndex = old }()

	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		input, _ := b["input"].(string)
		v := []float64{0.0, 0.0}
		if strings.Contains(strings.ToLower(input), "france") {
			v = []float64{1, 0}
		}
		if strings.Contains(strings.ToLower(input), "cake") {
			v = []float64{0, 1}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": v, "index": 0, "object": "embedding"}}})
	}))
	defer embedSrv.Close()

	cfg := Config{
		Backends:   map[string]Backend{"embed": {Name: "embed", BaseURL: embedSrv.URL}},
		Routes:     map[string]Route{"embed": {Name: "embed", Backend: "embed", Upstream: "emb"}},
		EmbedModel: "embed",
		ChunkSize:  1000,
		RetrieveK:  3,
	}

	n, err := cfg.ingestDocuments([]documentInput{
		{Name: "geo", Text: "France is a country in Europe."},
		{Name: "food", Text: "Cake is a sweet dessert."},
	})
	if err != nil || n != 2 {
		t.Fatalf("ingest n=%d err=%v", n, err)
	}

	hits, err := cfg.retrieve("Tell me about France", 3)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(hits) != 2 || hits[0].Text != "France is a country in Europe." {
		t.Errorf("retrieve hits = %+v", hits)
	}
}

func TestHandleChatRAG(t *testing.T) {
	old := docIndex
	docIndex = newVectorIndex(0)
	defer func() { docIndex = old }()

	var got []OpenAIMessage
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		input, _ := b["input"].(string)
		v := []float64{0.0, 0.0}
		if strings.Contains(strings.ToLower(input), "france") {
			v = []float64{1, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": v, "index": 0, "object": "embedding"}}})
	}))
	defer embedSrv.Close()

	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b OpenAIRequest
		json.NewDecoder(r.Body).Decode(&b)
		got = b.Messages
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}}})
	}))
	defer chatSrv.Close()

	cfg := Config{
		Backends: map[string]Backend{
			"embed": {Name: "embed", BaseURL: embedSrv.URL},
			"chat":  {Name: "chat", BaseURL: chatSrv.URL, Local: true},
		},
		Routes: map[string]Route{
			"embed":         {Name: "embed", Backend: "embed", Upstream: "emb"},
			"private:local": {Name: "private:local", Backend: "chat", Upstream: "m"},
		},
		EmbedModel: "embed",
		ChunkSize:  1000,
		RetrieveK:  3,
	}

	if _, err := cfg.ingestDocuments([]documentInput{{Name: "geo", Text: "France is a country in Europe."}}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	rec := httptest.NewRecorder()
	handleChat(rec, httptest.NewRequest("POST", "/api/chat",
		strings.NewReader(`{"model":"private:local","messages":[{"role":"user","content":"What about France?"}]}`)), cfg)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(got) == 0 || got[0].Role != "system" || !strings.Contains(got[0].Content, "France is a country") {
		t.Errorf("expected RAG context injected as system message, got %+v", got)
	}
}
