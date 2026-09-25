package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testConfig(baseURL string) Config {
	return Config{
		Port: "11434",
		Backends: map[string]Backend{
			"default": {Name: "default", BaseURL: baseURL, APIKey: "testkey"},
		},
		Default: "default",
		Routes: map[string]Route{
			"coach":           {Name: "coach", Backend: "default", Upstream: "deepseek-v4-flash"},
			"deepseek-v4-pro": {Name: "deepseek-v4-pro", Backend: "default", Upstream: "deepseek-v4-pro"},
		},
	}
}

func TestParseModels(t *testing.T) {
	got := parseModels("a,b=up1, c = up2 ", "default")
	want := []Route{
		{Name: "a", Backend: "default", Upstream: "a"},
		{Name: "b", Backend: "default", Upstream: "up1"},
		{Name: "c", Backend: "default", Upstream: "up2"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d models, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("model[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRoute(t *testing.T) {
	cfg := Config{
		Backends: map[string]Backend{
			"deepseek": {Name: "deepseek", BaseURL: "https://api.deepseek.com", Local: false},
			"thoth":    {Name: "thoth", BaseURL: "http://thoth.lab:8000/v1", Local: true},
		},
		Default: "deepseek",
		Routes: map[string]Route{
			"coach":         {Name: "coach", Backend: "deepseek", Upstream: "deepseek-v4-flash"},
			"private:llama": {Name: "private:llama", Backend: "thoth", Upstream: "llama3.1-70b"},
		},
	}

	b, up, err := cfg.route("coach")
	if err != nil || b.Name != "deepseek" || up != "deepseek-v4-flash" {
		t.Errorf("coach routed to %s/%s err=%v, want deepseek/deepseek-v4-flash", b.Name, up, err)
	}
	b, up, err = cfg.route("private:llama")
	if err != nil || b.Name != "thoth" || up != "llama3.1-70b" {
		t.Errorf("private:llama routed to %s/%s err=%v, want thoth/llama3.1-70b", b.Name, up, err)
	}
	b, up, err = cfg.route("unknown-model")
	if err != nil || b.Name != "deepseek" || up != "unknown-model" {
		t.Errorf("unknown routed to %s/%s err=%v, want deepseek/unknown-model", b.Name, up, err)
	}
}

func TestPrivateRouteRejectsCloud(t *testing.T) {
	cfg := Config{
		Backends: map[string]Backend{
			"deepseek": {Name: "deepseek", BaseURL: "https://api.deepseek.com", Local: false},
		},
		Default: "deepseek",
		Routes:  map[string]Route{},
	}
	// private model with no route falls through to the cloud default -> refused.
	if _, _, err := cfg.route("private:missing"); err == nil {
		t.Errorf("private:missing should be rejected (no local backend)")
	}
	// private model explicitly routed to a cloud backend -> refused.
	cfg.Routes["private:x"] = Route{Name: "private:x", Backend: "deepseek", Upstream: "deepseek-v4-flash"}
	if _, _, err := cfg.route("private:x"); err == nil {
		t.Errorf("private:x should be rejected (cloud backend)")
	}
}

func TestToOpenAIRequest(t *testing.T) {
	temp := 0.5
	topP := 0.8
	req := toOpenAIRequest("deepseek-v4-flash", []OpenAIMessage{{Role: "user", Content: "hi"}}, "json", &temp, &topP, 100)
	if req.Model != "deepseek-v4-flash" {
		t.Errorf("model not set: %s", req.Model)
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

	cfg := testConfig(mock.URL)
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
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer mock.Close()

	cfg := testConfig(mock.URL)
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"model":"coach","messages":[{"role":"user","content":"hi"}],"stream":true}`))
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
	cfg := testConfig("https://api.deepseek.com")

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
