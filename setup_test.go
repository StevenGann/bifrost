package main

import (
	"os"
	"testing"
)

// TestMain resets package-level singletons before the test suite runs so tests
// don't leak state into one another. The response cache in particular must
// start disabled (ttl 0) — cache tests opt in explicitly.
func TestMain(m *testing.M) {
	metrics = newMetrics(defaultPricing())
	governor = newGovernor(Config{})
	lruCache = newCache(0, 0)
	os.Exit(m.Run())
}
