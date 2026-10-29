package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDashboardRenders(t *testing.T) {
	m := newMetrics(defaultPricing())
	m.Record("coach", "deepseek-v4-flash", "patzer", "/api/chat", 200, 100, 50, 500*time.Millisecond, false)
	m.Record("deepseek-v4-pro", "deepseek-v4-pro", "unknown", "/v1/chat/completions", 502, 20, 0, 10*time.Millisecond, true)

	rec := httptest.NewRecorder()
	m.Dashboard().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<title>Bifrost</title>",
		`class="mono">coach</td>`,
		">patzer</td>",
		"total cost (USD)",
		"/v1/chat/completions",
		"/metrics",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard output missing %q", want)
		}
	}
	// The failed request should surface a 502 with the err styling.
	if !strings.Contains(body, `class="err">502`) {
		t.Errorf("dashboard missing the errored 502 row:\n%s", body)
	}
	// The page must not fall back to a whole-document reload: the live updater
	// polls /dashboard.json and patches the DOM in place.
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("dashboard still uses a meta refresh instead of in-place updates")
	}
	if !strings.Contains(body, "/dashboard.json") {
		t.Error("dashboard does not reference its live JSON endpoint")
	}
}

// TestDashboardJSON pins the shape the page's updater depends on.
func TestDashboardJSON(t *testing.T) {
	m := newMetrics(defaultPricing())
	m.Record("coach", "deepseek-v4-flash", "patzer", "/api/chat", 200, 100, 50, 500*time.Millisecond, false)
	m.RecordAudio("tts", "eleven_multilingual_v2", "patzer", "/v1/audio/speech", 200, 42, 0.00042, 800*time.Millisecond, false)

	rec := httptest.NewRecorder()
	m.DashboardJSON().ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard.json", nil))

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control %q, want no-store", cc)
	}
	var d dashboardData
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(d.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(d.Rows))
	}
	var sawTTS bool
	for _, r := range d.Rows {
		if r.Model == "tts" {
			sawTTS = true
			if r.Chars != 42 {
				t.Fatalf("tts chars = %d, want 42", r.Chars)
			}
			if r.TokensIn != 0 || r.TokensOut != 0 {
				t.Fatalf("speech row should carry no tokens, got %d/%d", r.TokensIn, r.TokensOut)
			}
		}
	}
	if !sawTTS {
		t.Fatal("speech row missing from dashboard data")
	}
	if d.TotalChars != 42 {
		t.Fatalf("total chars = %d, want 42", d.TotalChars)
	}
}
