package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckStateCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cfg := Config{
		LedgerFile: filepath.Join(dir, "spend.jsonl"),
		KeysFile:   filepath.Join(dir, "keys.json"),
	}
	if err := checkState(cfg); err != nil {
		t.Fatalf("checkState: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("directory was not created: %v", err)
	}
}

func TestCheckStateNoStateConfigured(t *testing.T) {
	if err := checkState(Config{}); err != nil {
		t.Fatalf("empty config should be a no-op: %v", err)
	}
}

func TestCheckStateRejectsDirectoryAsFile(t *testing.T) {
	dir := t.TempDir()
	occupied := filepath.Join(dir, "spend.jsonl")
	if err := os.Mkdir(occupied, 0o700); err != nil {
		t.Fatal(err)
	}
	err := checkState(Config{LedgerFile: occupied})
	if err == nil {
		t.Fatal("expected an error when the ledger path is a directory")
	}
}

func TestCheckStateRejectsUnwritableExistingFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to a 0400 file; permission checks are not meaningful")
	}
	dir := t.TempDir()
	ledger := filepath.Join(dir, "spend.jsonl")
	if err := os.WriteFile(ledger, []byte("{}\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := checkState(Config{LedgerFile: ledger}); err == nil {
		t.Fatal("expected an error for a read-only ledger")
	}
}

// The ledger must keep its history across restarts; this is the property the
// portability test exercises against a real container.
func TestLedgerAppendsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "spend.jsonl")
	g := newGovernor(Config{LedgerFile: ledger})
	g.Record("hermes", "deepseek-v4-flash", "deepseek-v4-flash", 0, 0, 0.25)

	got, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("ledger is empty after a record")
	}

	g2 := newGovernor(Config{LedgerFile: ledger})
	if g2.loadErr != nil {
		t.Fatalf("reload: %v", g2.loadErr)
	}
	if g2.monthSpend["hermes"] != 0.25 {
		t.Fatalf("reloaded spend = %v, want 0.25", g2.monthSpend["hermes"])
	}
}
