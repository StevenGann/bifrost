package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// checkState makes sure the paths Bifrost writes to are usable before the
// server starts.
//
// This runs at startup rather than on first write on purpose. A ledger
// directory that cannot be written used to fail one log line per request:
// spend silently stopped being recorded, budgets kept reading zero, and the
// only symptom was a log nobody was tailing. Under our cluster the PVC and
// fsGroup hid it; on a compose host it is the first thing that happens.
func checkState(cfg Config) error {
	for _, item := range []struct{ what, path string }{
		{"LEDGER_FILE", cfg.LedgerFile},
		{"KEYS_FILE", cfg.KeysFile},
	} {
		if item.path == "" {
			continue // feature not enabled: nothing to check
		}
		dir := filepath.Dir(item.path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("%s: cannot create %s: %w", item.what, dir, err)
		}
		f, err := os.CreateTemp(dir, ".bifrost-write-test-*")
		if err != nil {
			return fmt.Errorf("%s: %s is not writable by uid %d: %w", item.what, dir, os.Getuid(), err)
		}
		name := f.Name()
		f.Close()
		os.Remove(name)

		// An existing file can be read-only even when its directory is not.
		fi, err := os.Stat(item.path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("%s: %w", item.what, err)
		}
		if fi.IsDir() {
			return fmt.Errorf("%s: %s is a directory", item.what, item.path)
		}
		f, err = os.OpenFile(item.path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("%s: %s is not writable: %w", item.what, item.path, err)
		}
		f.Close()
	}
	return nil
}

// probeHealth backs the -health flag. A scratch image has no curl or wget, so
// the binary probes itself and docker/compose can use it as a HEALTHCHECK.
func probeHealth(port string) {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	os.Exit(0)
}
