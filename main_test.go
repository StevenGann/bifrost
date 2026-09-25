package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseModels(t *testing.T) {
	got := parseModels("a,b=up1, c = up2 ")
	want := []ModelAlias{{Name: "a", Upstream: "a"}, {Name: "b", Upstream: "up1"}, {Name: "c", Upstream: "up2"}}
	if len(got) != len(want) {
		t.Fatalf("got %d models, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("model[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestUpstreamModel(t *testing.T) {
	cfg := Config{Models: []ModelAlias{{Name: "coach", Upstream: "deepseek-v4-flash"}}}
	if cfg.upstreamModel("coach") != "deepseek-v4-flash" {
		t.Errorf("alias not mapped")
	}
	if cfg.upstreamModel("unknown") != "unknown" {
		t.Errorf("unknown name should pass through unchanged")
	}
}

func TestToOpenAIRequest(t *testing.T) {
	cfg := Config{Models: []ModelAlias{{Name: "coach", Upstream: "deepseek-v4-flash"}}}
	temp := 0.5
	topP := 0.8
	req := toOpenAIRequest(cfg, "coach", []OpenAIMessage{{Role: "user", Content: "hi"}}, "json", &temp, &topP, 100)
	if req.Model != "deepseek-v4-flash" {
		t.Errorf("model not mapped: %s", req.Model)
	}
	if req.Temperature != 0.5 || req.TopP != 0.8 {
		t.Errorf("options not passed through: %+v", req)
	}
	if req.MaxTokens != 100 {
		t.Errorf("num_predict not mapped to max_tokens: %d", req.MaxTokens)
	}
	if req.ResponseFormat == nil || req.ResponseFormat.Type != "json_object" {
		t.Errorf("format=json not mapped to response_format")
	}
}

func TestChatNonStream(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer testkey" {
			t.Errorf("missing/incorrect auth: %q", got)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "deepseek-v4-flash" {
			t.Errorf("model not remapped upstream: %v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "hello back"}}},
		})
	}))
	defer mock.Close()

	cfg := Config{UpstreamBase: mock.URL, UpstreamKey: "testkey", Models: []ModelAlias{{Name: "coach", Upstream: "deepseek-v4-flash"}}}

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
	if resp.Message.Content != "hello back" || !resp.Done || resp.Message.Role != "assistant" {
		t.Errorf("bad response: %+v", resp)
	}
}

func TestChatStreamTranslation(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// role marker (empty content), then content, then done
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer mock.Close()

	cfg := Config{UpstreamBase: mock.URL, UpstreamKey: "k", Models: []ModelAlias{{Name: "m", Upstream: "m"}}}

	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	rec := httptest.NewRecorder()
	handleChat(rec, req, cfg)

	var contents []string
	doneSeen := false
	for _, line := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		var chunk ollamaStreamChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			t.Fatalf("non-JSON stream line %q: %v", line, err)
		}
		if chunk.Message.Content != "" {
			contents = append(contents, chunk.Message.Content)
		}
		if chunk.Done {
			doneSeen = true
		}
	}
	if got := strings.Join(contents, ""); got != "hello" {
		t.Errorf("streamed %q, want %q", got, "hello")
	}
	if !doneSeen {
		t.Errorf("no final done:true chunk")
	}
}

func TestTagsAndModels(t *testing.T) {
	cfg := Config{Models: []ModelAlias{{Name: "a", Upstream: "a"}, {Name: "coach", Upstream: "deepseek-v4-flash"}}}

	rec := httptest.NewRecorder()
	handleTags(rec, cfg)
	var tags struct {
		Models []ollamaModel `json:"models"`
	}
	json.Unmarshal(rec.Body.Bytes(), &tags)
	if len(tags.Models) != 2 {
		t.Errorf("tags: got %d models, want 2", len(tags.Models))
	}

	rec2 := httptest.NewRecorder()
	handleOpenAIModels(rec2, cfg)
	if !strings.Contains(rec2.Body.String(), `"id":"coach"`) {
		t.Errorf("openai models missing alias: %s", rec2.Body.String())
	}
}
