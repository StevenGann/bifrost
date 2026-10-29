package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func speechConfig(backend Backend, routeName, upstream string) Config {
	return Config{
		Backends:    map[string]Backend{backend.Name: backend},
		Default:     backend.Name,
		AudioRoutes: map[string]Route{routeName: {Name: routeName, Backend: backend.Name, Upstream: upstream}},
		Voices:      map[string]string{"alloy": "voice-abc"},
	}
}

// TestSpeechElevenLabsTranslation pins the wire contract: Bifrost translates
// the OpenAI speech body into ElevenLabs' text-to-speech shape, injects the
// vendor header, and returns the audio bytes untouched.
func TestSpeechElevenLabsTranslation(t *testing.T) {
	var gotPath, gotQuery, gotKey, gotText, gotModel string
	audio := []byte("ID3fake-mp3-bytes")
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotKey = r.Header.Get("xi-api-key")
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotText, gotModel = body["text"], body["model_id"]
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(audio)
	}))
	defer mock.Close()

	cfg := speechConfig(Backend{Name: "el", BaseURL: mock.URL, Kind: "elevenlabs", APIKey: "test-key"}, "tts", "eleven_multilingual_v2")

	req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"tts","input":"Hello there","voice":"alloy","response_format":"mp3"}`))
	rec := httptest.NewRecorder()
	handleOpenAISpeech(rec, req, cfg)

	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != string(audio) {
		t.Fatalf("audio body %q, want %q", got, audio)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("content-type %q", ct)
	}
	if gotPath != "/v1/text-to-speech/voice-abc" {
		t.Fatalf("path %q, want /v1/text-to-speech/voice-abc", gotPath)
	}
	if gotQuery != "output_format=mp3_44100_128" {
		t.Fatalf("query %q", gotQuery)
	}
	if gotKey != "test-key" {
		t.Fatalf("xi-api-key %q", gotKey)
	}
	if gotText != "Hello there" || gotModel != "eleven_multilingual_v2" {
		t.Fatalf("upstream body text=%q model=%q", gotText, gotModel)
	}
}

// TestSpeechOpenAIKindPassthrough checks the other dialect: an OpenAI-shaped
// backend receives the caller's body unchanged apart from the route's upstream
// model, authenticated with a bearer token.
func TestSpeechOpenAIKindPassthrough(t *testing.T) {
	var gotPath, gotAuth string
	var body map[string]any
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("audio"))
	}))
	defer mock.Close()

	cfg := speechConfig(Backend{Name: "oai", BaseURL: mock.URL, Kind: "openai", APIKey: "sk-test"}, "tts", "gpt-4o-mini-tts")
	req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"tts","input":"Hi","voice":"alloy","response_format":"mp3"}`))
	rec := httptest.NewRecorder()
	handleOpenAISpeech(rec, req, cfg)

	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/audio/speech" {
		t.Fatalf("path %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("authorization %q", gotAuth)
	}
	if body["model"] != "gpt-4o-mini-tts" || body["input"] != "Hi" || body["voice"] != "voice-abc" {
		t.Fatalf("upstream body %v", body)
	}
}

// TestSpeechFallsBackOnUpstreamFailure covers the reason this endpoint exists
// in a gateway at all: a dead voice backend becomes a different voice instead
// of silence.
func TestSpeechFallsBackOnUpstreamFailure(t *testing.T) {
	primaryHits, secondaryHits := 0, 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		http.Error(w, "boom", 500)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryHits++
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("fallback-audio"))
	}))
	defer secondary.Close()

	cfg := Config{
		Backends: map[string]Backend{
			"dead": {Name: "dead", BaseURL: primary.URL, Kind: "elevenlabs", APIKey: "k"},
			"live": {Name: "live", BaseURL: secondary.URL, Kind: "elevenlabs", APIKey: "k"},
		},
		Default: "dead",
		AudioRoutes: map[string]Route{
			"tts":  {Name: "tts", Backend: "dead", Upstream: "eleven_multilingual_v2"},
			"tts2": {Name: "tts2", Backend: "live", Upstream: "eleven_multilingual_v2"},
		},
		Voices:    map[string]string{"alloy": "voice-abc"},
		Fallbacks: map[string]string{"tts": "tts2"},
	}

	req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"tts","input":"Hello","voice":"alloy"}`))
	rec := httptest.NewRecorder()
	handleOpenAISpeech(rec, req, cfg)

	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "fallback-audio" {
		t.Fatalf("body %q", rec.Body.String())
	}
	if primaryHits != 1 || secondaryHits != 1 {
		t.Fatalf("primary=%d secondary=%d, want 1/1", primaryHits, secondaryHits)
	}
}

// TestSpeechRejectsUnsupportedFormat refuses rather than silently substituting
// a container the caller did not ask for.
func TestSpeechRejectsUnsupportedFormat(t *testing.T) {
	cfg := speechConfig(Backend{Name: "el", BaseURL: "http://127.0.0.1:1", Kind: "elevenlabs", APIKey: "k"}, "tts", "eleven_multilingual_v2")
	req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"tts","input":"Hi","voice":"alloy","response_format":"wav"}`))
	rec := httptest.NewRecorder()
	handleOpenAISpeech(rec, req, cfg)

	if rec.Code != 400 {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "response_format") {
		t.Fatalf("body %s", rec.Body.String())
	}
}

// TestSpeechRequiresInput guards the empty-text case, which vendors bill for
// but cannot synthesize.
func TestSpeechRequiresInput(t *testing.T) {
	cfg := speechConfig(Backend{Name: "el", BaseURL: "http://127.0.0.1:1", Kind: "elevenlabs"}, "tts", "eleven_multilingual_v2")
	req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"tts","input":"   ","voice":"alloy"}`))
	rec := httptest.NewRecorder()
	handleOpenAISpeech(rec, req, cfg)
	if rec.Code != 400 {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if b, _ := io.ReadAll(rec.Body); !strings.Contains(string(b), "input is required") {
		t.Fatalf("body %s", b)
	}
}
