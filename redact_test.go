package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"email me at bob@example.com":      "email me at [REDACTED]",
		"call 555-123-4567 now":            "call [REDACTED] now",
		"card 4111-1111-1111-1111 ok":      "card [REDACTED] ok",
		"ip 192.168.1.100 is mine":         "ip [REDACTED] is mine",
		"ssn 123-45-6789":                  "ssn [REDACTED]",
		"token ghp_AbCdEfGhIjKlMnOpQrStUv": "token [REDACTED]",
		"no pii here":                      "no pii here",
	}
	for in, want := range cases {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaybeRedact(t *testing.T) {
	cloud := Backend{Name: "cloud", BaseURL: "https://api.deepseek.com", Local: false}
	local := Backend{Name: "local", BaseURL: "http://192.168.10.20:8000", Local: true}
	body := []byte(`{"model":"coach","messages":[{"role":"user","content":"hi bob@example.com call 555-123-4567"}]}`)

	got := maybeRedact(cloud, body)
	if strings.Contains(string(got), "bob@example.com") || strings.Contains(string(got), "555-123-4567") {
		t.Errorf("cloud body not redacted: %s", got)
	}
	got = maybeRedact(local, body)
	if !strings.Contains(string(got), "bob@example.com") {
		t.Errorf("local body should not be redacted: %s", got)
	}
}

func TestChatRedactsCloudBackend(t *testing.T) {
	var upstreamContent string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		msgs := body["messages"].([]any)
		upstreamContent = msgs[0].(map[string]any)["content"].(string)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}}})
	}))
	defer mock.Close()

	cfg := testConfig(mock.URL)
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"model":"coach","messages":[{"role":"user","content":"my email bob@example.com"}]}`))
	rec := httptest.NewRecorder()
	handleChat(rec, req, cfg)

	if strings.Contains(upstreamContent, "bob@example.com") {
		t.Errorf("email leaked to cloud backend: %q", upstreamContent)
	}
	if !strings.Contains(upstreamContent, "[REDACTED]") {
		t.Errorf("expected redaction marker in %q", upstreamContent)
	}
}
