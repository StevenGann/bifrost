package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestGovernorBudget(t *testing.T) {
	g := newGovernor(Config{Budget: 1.0})
	if err := g.Check("jeeves"); err != nil {
		t.Fatalf("expected under budget, got %v", err)
	}
	g.Record("jeeves", "deepseek-v4-flash", 0.6)
	g.Record("jeeves", "deepseek-v4-flash", 0.5) // total 1.1 > 1.0
	if err := g.Check("jeeves"); err == nil {
		t.Fatal("expected over-budget refusal")
	} else if le, ok := err.(*limitError); !ok || le.Status() != 402 {
		t.Fatalf("expected 402 limitError, got %v", err)
	}
}

func TestGovernorPerAppBudget(t *testing.T) {
	g := newGovernor(Config{AppBudgets: map[string]float64{"patzer": 0.5}})
	g.Record("patzer", "deepseek-v4-flash", 0.5)
	if err := g.Check("patzer"); err == nil {
		t.Fatal("expected patzer over its per-app budget")
	} else if le, ok := err.(*limitError); !ok || le.Status() != 402 {
		t.Fatalf("expected 402, got %v", err)
	}
	if err := g.Check("jeeves"); err != nil {
		t.Fatalf("jeeves should be unaffected: %v", err)
	}
}

func TestGovernorRateLimit(t *testing.T) {
	g := newGovernor(Config{RateLimit: 1})
	if err := g.Check("jeeves"); err != nil {
		t.Fatalf("first request should pass: %v", err)
	}
	if err := g.Check("jeeves"); err == nil {
		t.Fatal("second request should be rate-limited")
	} else if le, ok := err.(*limitError); !ok || le.Status() != 429 {
		t.Fatalf("expected 429, got %v", err)
	}
}

func TestGovernorAuth(t *testing.T) {
	g := newGovernor(Config{AppKeys: map[string]string{"jeeves": "key-abc"}})

	r := httptest.NewRequest("POST", "/api/chat", nil)
	r.Header.Set("Authorization", "Bearer key-abc")
	if app, err := g.Authenticate(r); err != nil || app != "jeeves" {
		t.Fatalf("got app=%q err=%v", app, err)
	}

	if _, err := g.Authenticate(httptest.NewRequest("POST", "/api/chat", nil)); err == nil {
		t.Fatal("expected missing-token error")
	} else if le, ok := err.(*limitError); !ok || le.Status() != 401 {
		t.Fatalf("expected 401, got %v", err)
	}

	r3 := httptest.NewRequest("POST", "/api/chat", nil)
	r3.Header.Set("Authorization", "Bearer wrong")
	if _, err := g.Authenticate(r3); err == nil {
		t.Fatal("expected invalid-token error")
	}

	// No keys configured -> header fallback, backward-compatible.
	g2 := newGovernor(Config{})
	r4 := httptest.NewRequest("POST", "/api/chat", nil)
	r4.Header.Set("X-Bifrost-App", "patzer")
	if app, err := g2.Authenticate(r4); err != nil || app != "patzer" {
		t.Fatalf("header fallback: app=%q err=%v", app, err)
	}
}

func TestGovernorLedgerPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spend.jsonl")
	g := newGovernor(Config{LedgerFile: path})
	g.Record("jeeves", "deepseek-v4-flash", 0.123)
	g.Record("patzer", "deepseek-v4-flash", 0.456)

	g2 := newGovernor(Config{LedgerFile: path})
	if got := g2.monthSpend["jeeves"]; math.Abs(got-0.123) > 1e-9 {
		t.Fatalf("jeeves spend = %v, want 0.123", got)
	}
	if got := g2.monthTotal; math.Abs(got-0.579) > 1e-9 {
		t.Fatalf("total = %v, want 0.579", got)
	}
}

func TestGovernorMiddlewareEnforcesBudget(t *testing.T) {
	g := newGovernor(Config{Budget: 0.10})
	g.Record("jeeves", "deepseek-v4-flash", 0.10) // exhaust the budget

	called := false
	h := g.Wrap(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})
	req := httptest.NewRequest("POST", "/api/chat", nil)
	req.Header.Set("X-Bifrost-App", "jeeves")
	rr := httptest.NewRecorder()
	h(rr, req)

	if called {
		t.Fatal("handler must not run when over budget")
	}
	if rr.Code != 402 {
		t.Fatalf("got status %d, want 402", rr.Code)
	}
}

func TestGovernorMiddlewareSetsAppContext(t *testing.T) {
	g := newGovernor(Config{})
	seen := ""
	h := g.Wrap(func(w http.ResponseWriter, r *http.Request) {
		seen = appFrom(r)
		w.WriteHeader(200)
	})
	req := httptest.NewRequest("POST", "/api/chat", nil)
	req.Header.Set("X-Bifrost-App", "coach")
	rr := httptest.NewRecorder()
	h(rr, req)
	if seen != "coach" {
		t.Fatalf("handler saw app %q, want coach", seen)
	}
}
