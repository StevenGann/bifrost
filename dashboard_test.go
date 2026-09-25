package main

import (
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
}
