package main

import (
	"os"
	"testing"
	"time"
)

// TestMain resets package-level singletons before the test suite runs so tests
// don't leak state into one another. The response cache in particular must
// start disabled (ttl 0) — cache tests opt in explicitly. Circuits use a
// threshold of 1000 so integration tests never trip a breaker.
func TestMain(m *testing.M) {
	metrics = newMetrics(defaultPricing())
	governor = newGovernor(Config{})
	lruCache = newCache(0, 0)
	circuits = newCircuits(1000, time.Hour)
	os.Exit(m.Run())
}
