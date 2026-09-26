package main

import (
	"net/http"
	"sync"
	"time"
)

// Circuit is a per-backend circuit breaker. After `threshold` consecutive
// failures it opens: requests fail fast (503) without touching the network.
// After `cooldown` it half-opens, admitting a single probe that re-closes it on
// success. Failures are transport errors, 429s, and 5xx.
type Circuit struct {
	mu        sync.Mutex
	failures  int
	openedAt  time.Time
	threshold int
	cooldown  time.Duration
}

func (c *Circuit) allow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failures < c.threshold {
		return true
	}
	if c.cooldown > 0 && time.Since(c.openedAt) >= c.cooldown {
		c.openedAt = time.Now() // reset the window: only one probe gets through
		return true
	}
	return false
}

// recordFailure registers a failure and reports whether it opened the circuit
// (i.e. this failure crossed the threshold).
func (c *Circuit) recordFailure() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures++
	if c.failures == c.threshold {
		c.openedAt = time.Now()
		return true
	}
	return false
}

func (c *Circuit) recordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures = 0
	c.openedAt = time.Time{}
}

// markDown force-opens the circuit (the health poller confirmed the backend is
// unreachable). It reports whether the circuit newly opened. When already open,
// it refreshes the open window so the cooldown probe can't fire mid-outage.
func (c *Circuit) markDown() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	wasOpen := c.failures >= c.threshold
	c.failures = c.threshold
	c.openedAt = time.Now()
	return !wasOpen
}

// markUp half-opens a tripped circuit: the next request is admitted as a probe.
// A no-op while the circuit is closed (don't nudge a healthy circuit toward
// opening).
func (c *Circuit) markUp() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failures >= c.threshold {
		c.failures = c.threshold - 1
		c.openedAt = time.Time{}
	}
}

func (c *Circuit) open() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failures >= c.threshold
}

// circuitSet is the process-wide registry of per-backend circuit breakers.
type circuitSet struct {
	mu        sync.Mutex
	m         map[string]*Circuit
	threshold int
	cooldown  time.Duration
}

func newCircuits(threshold int, cooldown time.Duration) *circuitSet {
	if threshold <= 0 {
		threshold = 3
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &circuitSet{m: map[string]*Circuit{}, threshold: threshold, cooldown: cooldown}
}

func (cs *circuitSet) forName(name string) *Circuit {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if c, ok := cs.m[name]; ok {
		return c
	}
	c := &Circuit{threshold: cs.threshold, cooldown: cs.cooldown}
	cs.m[name] = c
	return c
}

func (cs *circuitSet) recordFailure(name string) bool { return cs.forName(name).recordFailure() }
func (cs *circuitSet) recordSuccess(name string)      { cs.forName(name).recordSuccess() }
func (cs *circuitSet) allow(name string) bool         { return cs.forName(name).allow() }
func (cs *circuitSet) markDown(name string) bool      { return cs.forName(name).markDown() }
func (cs *circuitSet) markUp(name string)             { cs.forName(name).markUp() }

// state returns the open/closed status of every backend seen so far.
func (cs *circuitSet) state() map[string]bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make(map[string]bool, len(cs.m))
	for name, c := range cs.m {
		out[name] = c.open()
	}
	return out
}

// circuits is the shared registry: initialized to defaults, replaced by main()
// from config, and reset by tests.
var circuits = newCircuits(3, 30*time.Second)

// do performs an upstream request through the backend's circuit breaker. On
// error it returns errCircuitOpen (tripped breaker) or an *upstreamError with
// status 0 (transport failure); non-transient HTTP responses pass through.
func do(b Backend, req *http.Request) (*http.Response, error) {
	if !circuits.allow(b.Name) {
		return nil, errCircuitOpen
	}
	client := httpClient
	if b.Local {
		client = httpClientLocal
	}
	resp, err := client.Do(req)
	if err != nil {
		if circuits.recordFailure(b.Name) {
			metrics.RecordCircuitTrip(b.Name)
		}
		return nil, &upstreamError{0, err.Error()}
	}
	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		if circuits.recordFailure(b.Name) {
			metrics.RecordCircuitTrip(b.Name)
		}
	} else {
		circuits.recordSuccess(b.Name)
	}
	return resp, nil
}
