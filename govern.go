package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
const identityCtxKey ctxKey = "bifrost-identity"

// Identity is who is calling: a named tenant backed by a key, or an anonymous
// caller with no credential.
type Identity struct {
	Name  string // label used in metrics, the ledger and per-app budgets
	KeyID string // "" when anonymous or when the caller used a legacy header
	Anon  bool
	Addr  string // source IP, used to bucket anonymous rate limits
}

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
	Ts    string `json:"ts"`
	App   string `json:"app"`
	Key   string `json:"key,omitempty"`
	Model string `json:"model"`           // upstream model, the thing that is billed
	Route string `json:"route,omitempty"` // client-facing name the caller asked for
	// Upstream side: which credential paid for this request. Never secret.
	Backend string  `json:"backend,omitempty"`
	UpKey   string  `json:"upstream_key,omitempty"`
	Plan    string  `json:"plan,omitempty"`
	In      int     `json:"tokens_in,omitempty"`
	Out     int     `json:"tokens_out,omitempty"`
	Cost    float64 `json:"cost"`
	Chars   int     `json:"chars,omitempty"`
}

// Governor enforces auth, rate limits, and monthly spend budgets, backed by a
// durable JSONL ledger so budgets survive restarts.
type Governor struct {
	mu         sync.Mutex
	path       string
	budget     float64
	appBudgets map[string]float64
	rateLimit  int
	buckets    map[string]*tokenBucket
	month      string
	monthSpend map[string]float64
	monthLines []ledgerLine
	monthTotal float64

	// Tenant registry. Keys are minted here; APP_KEYS entries are imported as
	// static keys so existing deployments keep working.
	store      *keyStore
	storeErr   error
	loadErr    error
	adminKey   string
	anon       string // "open", "limited" or "closed"
	anonRate   int
	anonBudget float64
}

// governor is the package-level enforcer, a no-op default here, replaced by
// main() with the env-configured instance.
var governor = newGovernor(Config{})

func newGovernor(cfg Config) *Governor {
	g := &Governor{
		path:       cfg.LedgerFile,
		budget:     cfg.Budget,
		appBudgets: cfg.AppBudgets,
		rateLimit:  cfg.RateLimit,
		buckets:    map[string]*tokenBucket{},
		month:      monthKey(time.Now()),
		monthSpend: map[string]float64{},
		adminKey:   cfg.AdminKey,
		anon:       cfg.Anon,
		anonRate:   cfg.AnonRate,
		anonBudget: cfg.AnonBudget,
	}
	if g.anon == "" {
		// Turn on keys and you have opted into authentication; a keyless
		// deployment keeps the old permissive behaviour.
		if len(cfg.AppKeys) > 0 || cfg.KeysFile != "" {
			g.anon = "closed"
		} else {
			g.anon = "open"
		}
	}
	store, err := newKeyStore(cfg.KeysFile)
	if err != nil {
		// Keep serving, but deny: a corrupt registry must not become a policy
		// that lets everyone in. main() refuses to start on this error.
		g.storeErr = err
		store, _ = newKeyStore("")
	}
	g.store = store
	for app, key := range cfg.AppKeys {
		g.store.addStatic(app, key)
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
		if !errors.Is(err, os.ErrNotExist) {
			g.loadErr = err
		}
		return
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
			g.monthLines = append(g.monthLines, l)
		}
	}
}

// LedgerThisMonth returns the ledger entries for the current UTC month, in the
// order they were written. The dashboard uses it to rebuild its month totals on
// start-up: the metrics counters are process-scoped, so without this a restart
// made the month's spend look as if it had never happened.
func (g *Governor) LedgerThisMonth() []ledgerLine {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]ledgerLine, len(g.monthLines))
	copy(out, g.monthLines)
	return out
}

// Month is the UTC month the governor is currently accounting for.
func (g *Governor) Month() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.month
}

func monthOf(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	return monthKey(t)
}

// Authenticate resolves who is calling. A presented token must be valid: a
// wrong key is an error, never a silent downgrade to anonymous.
func (g *Governor) Authenticate(r *http.Request) (Identity, error) {
	if token := bearerToken(r); token != "" {
		k, err := g.store.lookup(token)
		if err != nil {
			return Identity{}, &limitError{401, "invalid bearer token"}
		}
		g.store.touch(k.ID)
		return Identity{Name: k.Name, KeyID: k.ID}, nil
	}
	if g.anon == "closed" {
		return Identity{}, &limitError{401, "bearer token required"}
	}
	// With no keys minted at all, X-Bifrost-App is still honoured as a label so
	// a keyless deployment behaves exactly as it did before 0.18.
	if g.store.count() == 0 {
		if a := strings.TrimSpace(r.Header.Get("X-Bifrost-App")); a != "" {
			return Identity{Name: a}, nil
		}
	}
	return Identity{Name: "anonymous", Anon: true, Addr: clientIP(r)}, nil
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return ""
}

// Check enforces the rate limit and monthly budget for the calling identity.
// A key carries its own limits; anonymous callers get theirs from ANON_*.
func (g *Governor) Check(id Identity) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rolloverLocked()

	rate := g.rateLimit
	budget := g.budget
	appBudget := g.appBudgets[id.Name]

	if id.Anon {
		if g.anonRate > 0 {
			rate = g.anonRate
		}
		if g.anonBudget > 0 {
			appBudget = g.anonBudget
		}
	} else if k := g.store.get(id.KeyID); k != nil {
		if k.RateLimit > 0 {
			rate = k.RateLimit
		}
		if k.Budget > 0 {
			appBudget = k.Budget
		}
	}

	if rate > 0 {
		bucket := id.KeyID
		if bucket == "" {
			bucket = "name:" + id.Name
		}
		if id.Anon {
			bucket = "anon:" + id.Addr
		}
		b := g.buckets[bucket]
		if b == nil {
			b = newTokenBucket(float64(rate))
			g.buckets[bucket] = b
		}
		if !b.allow() {
			return &limitError{429, fmt.Sprintf("rate limit exceeded for %q", id.Name)}
		}
	}

	if budget > 0 && g.monthTotal >= budget {
		return &limitError{402, fmt.Sprintf("monthly budget $%.2f exceeded", budget)}
	}
	if appBudget > 0 && g.monthSpend[id.Name] >= appBudget {
		return &limitError{402, fmt.Sprintf("%q budget $%.2f exceeded", id.Name, appBudget)}
	}
	return nil
}

// Record adds cost to the current month and appends it to the ledger.
func (g *Governor) Record(app, route, model string, tokensIn, tokensOut int, cost float64, up Served) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rolloverLocked()
	g.monthSpend[app] += cost
	g.monthTotal += cost
	g.appendLocked(app, route, model, tokensIn, tokensOut, cost, up)
}

// RecordAudio books a speech request: characters synthesized and their cost.
// Speech is priced per character, so it lands in the same ledger with the
// character count carried alongside the cost.
func (g *Governor) RecordAudio(app, model string, chars int, cost float64, up Served) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rolloverLocked()
	g.monthSpend[app] += cost
	g.monthTotal += cost
	g.appendAudioLocked(app, model, chars, cost, up)
}

func (g *Governor) appendAudioLocked(app, model string, chars int, cost float64, up Served) {
	if g.path == "" {
		return
	}
	line := ledgerLine{
		Ts:      time.Now().UTC().Format(time.RFC3339),
		App:     app,
		Key:     g.store.idForName(app),
		Model:   model,
		Cost:    cost,
		Chars:   chars,
		Backend: up.Backend,
		UpKey:   up.Key,
		Plan:    up.Plan,
	}
	g.monthLines = append(g.monthLines, line)
	b, err := json.Marshal(line)
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

func (g *Governor) rolloverLocked() {
	mk := monthKey(time.Now())
	if mk != g.month {
		g.month = mk
		g.monthSpend = map[string]float64{}
		g.monthTotal = 0
	}
}

func (g *Governor) appendLocked(app, route, model string, tokensIn, tokensOut int, cost float64, up Served) {
	if g.path == "" {
		return
	}
	line := ledgerLine{
		Ts:      time.Now().UTC().Format(time.RFC3339),
		App:     app,
		Key:     g.store.idForName(app),
		Model:   model,
		Route:   route,
		In:      tokensIn,
		Out:     tokensOut,
		Cost:    cost,
		Backend: up.Backend,
		UpKey:   up.Key,
		Plan:    up.Plan,
	}
	g.monthLines = append(g.monthLines, line)
	b, err := json.Marshal(line)
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
// and makes the authenticated identity available to the handler via context.
func (g *Governor) Wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id, err := g.Authenticate(r)
		if err != nil {
			status := 401
			if le, ok := err.(*limitError); ok {
				status = le.Status()
			}
			metrics.Record("unauthorized", "unauthorized", "unauthorized", r.URL.Path, status, 0, 0, time.Since(start), true, Served{})
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		if err := g.Check(id); err != nil {
			status := 500
			if le, ok := err.(*limitError); ok {
				status = le.Status()
			}
			metrics.Record(id.Name, id.Name, id.Name, r.URL.Path, status, 0, 0, time.Since(start), true, Served{})
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		ctx := context.WithValue(r.Context(), appCtxKey, id.Name)
		ctx = context.WithValue(ctx, identityCtxKey, id)
		next(w, r.WithContext(ctx))
	}
}
