package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type ctxKey string

const appCtxKey ctxKey = "bifrost-app"

// limitError is a governance refusal (auth, rate limit, or budget).
type limitError struct {
	status int
	msg    string
}

func (e *limitError) Error() string { return e.msg }
func (e *limitError) Status() int   { return e.status }

// tokenBucket is a simple per-app rate limiter (requests/minute, burstable to
// the full minute allowance).
type tokenBucket struct {
	capacity float64
	rate     float64 // tokens/sec
	tokens   float64
	last     time.Time
}

func newTokenBucket(perMinute float64) *tokenBucket {
	return &tokenBucket{capacity: perMinute, rate: perMinute / 60, tokens: perMinute, last: time.Now()}
}

func (b *tokenBucket) allow() bool {
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * b.rate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

type ledgerLine struct {
	Ts    string  `json:"ts"`
	App   string  `json:"app"`
	Model string  `json:"model"`
	Cost  float64 `json:"cost"`
}

// Governor enforces auth, rate limits, and monthly spend budgets, backed by a
// durable JSONL ledger so budgets survive restarts.
type Governor struct {
	mu         sync.Mutex
	path       string
	budget     float64
	appBudgets map[string]float64
	keys       map[string]string // bearer key -> app
	rateLimit  int
	buckets    map[string]*tokenBucket
	month      string
	monthSpend map[string]float64
	monthTotal float64
}

// governor is the package-level enforcer, a no-op default here, replaced by
// main() with the env-configured instance.
var governor = newGovernor(Config{})

func newGovernor(cfg Config) *Governor {
	g := &Governor{
		path:       cfg.LedgerFile,
		budget:     cfg.Budget,
		appBudgets: cfg.AppBudgets,
		keys:       map[string]string{},
		rateLimit:  cfg.RateLimit,
		buckets:    map[string]*tokenBucket{},
		month:      monthKey(time.Now()),
		monthSpend: map[string]float64{},
	}
	// APP_KEYS is declared app->key; invert to key->app for O(1) lookup.
	for app, key := range cfg.AppKeys {
		if key != "" {
			g.keys[key] = app
		}
	}
	g.load()
	return g
}

func monthKey(t time.Time) string { return t.UTC().Format("2006-01") }

func (g *Governor) load() {
	if g.path == "" {
		return
	}
	f, err := os.Open(g.path)
	if err != nil {
		return // no ledger yet
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l ledgerLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if monthOf(l.Ts) == g.month {
			g.monthSpend[l.App] += l.Cost
			g.monthTotal += l.Cost
		}
	}
}

func monthOf(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	return monthKey(t)
}

// Authenticate resolves the caller's app identity. When APP_KEYS is configured
// a Bearer token is required; otherwise the self-reported X-Bifrost-App header
// is used (spoofable, but backward-compatible).
func (g *Governor) Authenticate(r *http.Request) (string, error) {
	if len(g.keys) == 0 {
		if a := strings.TrimSpace(r.Header.Get("X-Bifrost-App")); a != "" {
			return a, nil
		}
		return "unknown", nil
	}
	token := bearerToken(r)
	if token == "" {
		return "", &limitError{401, "missing bearer token"}
	}
	app, ok := g.keys[token]
	if !ok {
		return "", &limitError{401, "invalid bearer token"}
	}
	return app, nil
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return ""
}

// Check enforces the rate limit and monthly budget for app.
func (g *Governor) Check(app string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rolloverLocked()

	if g.rateLimit > 0 {
		b := g.buckets[app]
		if b == nil {
			b = newTokenBucket(float64(g.rateLimit))
			g.buckets[app] = b
		}
		if !b.allow() {
			return &limitError{429, fmt.Sprintf("rate limit exceeded for app %q", app)}
		}
	}

	if g.budget > 0 && g.monthTotal >= g.budget {
		return &limitError{402, fmt.Sprintf("monthly budget $%.2f exceeded", g.budget)}
	}
	if b, ok := g.appBudgets[app]; ok && b > 0 && g.monthSpend[app] >= b {
		return &limitError{402, fmt.Sprintf("app %q budget $%.2f exceeded", app, b)}
	}
	return nil
}

// Record adds cost to the current month and appends it to the ledger.
func (g *Governor) Record(app, model string, cost float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rolloverLocked()
	g.monthSpend[app] += cost
	g.monthTotal += cost
	g.appendLocked(app, model, cost)
}

func (g *Governor) rolloverLocked() {
	mk := monthKey(time.Now())
	if mk != g.month {
		g.month = mk
		g.monthSpend = map[string]float64{}
		g.monthTotal = 0
	}
}

func (g *Governor) appendLocked(app, model string, cost float64) {
	if g.path == "" {
		return
	}
	b, err := json.Marshal(ledgerLine{Ts: time.Now().UTC().Format(time.RFC3339), App: app, Model: model, Cost: cost})
	if err != nil {
		return
	}
	f, err := os.OpenFile(g.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("governor: ledger append failed: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		log.Printf("governor: ledger write failed: %v", err)
	}
}

// Wrap guards a completion handler with auth, rate-limit, and budget checks,
// and makes the authenticated app available to the handler via context.
func (g *Governor) Wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		app, err := g.Authenticate(r)
		if err != nil {
			if app == "" {
				app = "unknown"
			}
			metrics.Record(app, app, app, r.URL.Path, 401, 0, 0, time.Since(start), true)
			writeJSON(w, 401, map[string]string{"error": err.Error()})
			return
		}
		if err := g.Check(app); err != nil {
			status := 500
			if le, ok := err.(*limitError); ok {
				status = le.Status()
			}
			metrics.Record(app, app, app, r.URL.Path, status, 0, 0, time.Since(start), true)
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), appCtxKey, app)))
	}
}
