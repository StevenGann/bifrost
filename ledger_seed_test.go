package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The bug this guards: the dashboard reads process-scoped counters, so after a
// restart it reported the month's spend as zero even though the ledger still
// held it. Seeding from the ledger is what makes a restart invisible.
func TestDashboardSeededFromLedger(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "spend.jsonl")

	now := time.Now().UTC()
	prev := now.AddDate(0, -1, 0)
	lines := []string{
		fmt.Sprintf(`{"ts":%q,"app":"hermes","key":"926060c3","model":"deepseek-v4-flash","route":"coach","tokens_in":100,"tokens_out":20,"cost":0.5}`, now.Format(time.RFC3339)),
		fmt.Sprintf(`{"ts":%q,"app":"hermes","key":"926060c3","model":"deepseek-v4-flash","route":"coach","tokens_in":50,"tokens_out":10,"cost":0.25}`, now.Format(time.RFC3339)),
		// Previous month: history, not this month's budget or totals.
		fmt.Sprintf(`{"ts":%q,"app":"hermes","model":"deepseek-v4-flash","cost":9.0}`, prev.Format(time.RFC3339)),
	}
	if err := os.WriteFile(ledger, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	g := newGovernor(Config{LedgerFile: ledger})
	if g.loadErr != nil {
		t.Fatalf("load: %v", g.loadErr)
	}
	if got := len(g.LedgerThisMonth()); got != 2 {
		t.Fatalf("ledger lines for this month = %d, want 2", got)
	}

	m := newMetrics(defaultPricing())
	m.SeedFromLedger(g.Month(), g.LedgerThisMonth())
	d := m.dashboardData()

	if d.TotalRequests != 2 {
		t.Errorf("TotalRequests = %d, want 2", d.TotalRequests)
	}
	if math.Abs(d.TotalCostUSD-0.75) > 1e-9 {
		t.Errorf("TotalCostUSD = %v, want 0.75", d.TotalCostUSD)
	}
	if d.TotalTokensIn != 150 || d.TotalTokensOut != 30 {
		t.Errorf("tokens = %d/%d, want 150/30", d.TotalTokensIn, d.TotalTokensOut)
	}
	if len(d.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(d.Rows))
	}
	if d.Rows[0].Model != "coach" || d.Rows[0].App != "hermes" {
		t.Errorf("row = %s/%s, want coach/hermes", d.Rows[0].Model, d.Rows[0].App)
	}
}

// A month boundary can pass while the process is running. Budgets roll over in
// the governor; the dashboard seed has to roll over with them, or November 1st
// would report October's money as November's.
func TestStaleSeedIsDroppedAtMonthBoundary(t *testing.T) {
	m := newMetrics(defaultPricing())
	lastMonth := monthKey(time.Now().AddDate(0, -1, 0))
	m.SeedFromLedger(lastMonth, []ledgerLine{
		{Ts: time.Now().Format(time.RFC3339), App: "hermes", Model: "m", Cost: 12.5},
	})

	d := m.dashboardData()
	if d.TotalRequests != 0 || d.TotalCostUSD != 0 {
		t.Fatalf("stale seed still counted: requests=%d cost=%v", d.TotalRequests, d.TotalCostUSD)
	}
	if m.seedMonth != "" {
		t.Errorf("seedMonth not cleared: %q", m.seedMonth)
	}
}

// A request made after start-up must add to the seeded baseline, not replace
// or double it.
func TestSeededTotalsAccumulate(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "spend.jsonl")
	now := time.Now().UTC().Format(time.RFC3339)
	seed := fmt.Sprintf(`{"ts":%q,"app":"hermes","model":"deepseek-v4-flash","route":"coach","tokens_in":100,"tokens_out":20,"cost":1.0}`+"\n", now)
	if err := os.WriteFile(ledger, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	g := newGovernor(Config{LedgerFile: ledger})
	m := newMetrics(defaultPricing())
	m.SeedFromLedger(g.Month(), g.LedgerThisMonth())

	// A live request through the normal recording path.
	m.Record("coach", "deepseek-v4-flash", "hermes", "/v1/chat/completions", 200, 10, 5, time.Millisecond, false, Served{})

	d := m.dashboardData()
	if d.TotalRequests != 2 {
		t.Errorf("TotalRequests = %d, want 2 (1 seeded + 1 live)", d.TotalRequests)
	}
	// The live request adds its own (tiny) computed cost on top of the seed.
	if d.TotalCostUSD <= 1.0 {
		t.Errorf("TotalCostUSD = %v, want more than the 1.0 seed", d.TotalCostUSD)
	}
	if d.TotalTokensIn != 110 {
		t.Errorf("TotalTokensIn = %d, want 110", d.TotalTokensIn)
	}
}

// The ledger has to carry the client-facing route and the token counts, or the
// dashboard cannot be rebuilt from it at all.
func TestLedgerLineCarriesRouteAndTokens(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "spend.jsonl")
	g := newGovernor(Config{LedgerFile: ledger})
	g.Record("hermes", "coach", "deepseek-v4-flash", 123, 45, 0.01, Served{})

	b, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"route":"coach"`, `"tokens_in":123`, `"tokens_out":45`, `"model":"deepseek-v4-flash"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("ledger line missing %s: %s", want, string(b))
		}
	}
}

// The Sankey is only honest if the ledger line carries both axes. This guards
// the fold: who called, which upstream credential paid, and that the two maps
// (ledger seed + this process) are summed rather than overwriting each other.
func TestFlowsCarryBothAxes(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "spend.jsonl")

	now := time.Now().UTC()
	ts := now.Format(time.RFC3339)
	lines := []string{
		fmt.Sprintf(`{"ts":%q,"app":"hermes","backend":"deepseek","upstream_key":"primary","plan":"pro","tokens_in":100,"tokens_out":20,"cost":0.5}`, ts),
		fmt.Sprintf(`{"ts":%q,"app":"hermes","backend":"deepseek","upstream_key":"primary","plan":"pro","tokens_in":40,"tokens_out":10,"cost":0.25}`, ts),
		fmt.Sprintf(`{"ts":%q,"app":"hermes","backend":"deepseek","upstream_key":"backup","plan":"free","tokens_in":10,"tokens_out":2,"cost":0.05}`, ts),
		fmt.Sprintf(`{"ts":%q,"app":"coach","backend":"epsilon","tokens_in":0,"tokens_out":0,"cost":0}`, ts),
		// Pre-fix line: no upstream recorded, must land in the unknown bucket
		// instead of vanishing or inventing a backend.
		fmt.Sprintf(`{"ts":%q,"app":"legacy","tokens_in":5,"tokens_out":1,"cost":0.01}`, ts),
	}
	if err := os.WriteFile(ledger, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	g := newGovernor(Config{LedgerFile: ledger})
	if g.loadErr != nil {
		t.Fatalf("load: %v", g.loadErr)
	}
	m := newMetrics(defaultPricing())
	m.SeedFromLedger(g.Month(), g.LedgerThisMonth())

	// A request handled by this process must add to the same picture.
	// Record prices from tokens, so keep its own answer rather than assuming a
	// number: the point is that this line lands on the same flow.
	live := m.Record("deepseek-v4-flash", "deepseek-v4-flash", "hermes", "chat", 200, 1000, 500, time.Millisecond, false,
		Served{Backend: "deepseek", Key: "primary", Plan: "pro"})
	if live <= 0 {
		t.Fatalf("live request booked no cost; pricing has no entry for deepseek-v4-flash")
	}

	d := m.dashboardData()
	type pair struct{ client, up string }
	got := map[pair]flowRow{}
	for _, f := range d.Flows {
		got[pair{f.Client, f.Upstream}] = f
	}

	want := map[pair]struct {
		cost     float64
		requests uint64
	}{
		{"hermes", "deepseek/primary"}: {0.75 + live, 3},
		{"hermes", "deepseek/backup"}:  {0.05, 1},
		{"coach", "epsilon"}:           {0.0, 1},
		{"legacy", "unknown"}:          {0.01, 1},
	}
	if len(got) != len(want) {
		t.Fatalf("flows = %d, want %d: %+v", len(got), len(want), d.Flows)
	}
	for k, w := range want {
		f, ok := got[k]
		if !ok {
			t.Errorf("missing flow %s -> %s", k.client, k.up)
			continue
		}
		if math.Abs(f.CostUSD-w.cost) > 1e-9 {
			t.Errorf("%s -> %s cost = %v, want %v", k.client, k.up, f.CostUSD, w.cost)
		}
		if f.Requests != w.requests {
			t.Errorf("%s -> %s requests = %d, want %d", k.client, k.up, f.Requests, w.requests)
		}
	}
}
