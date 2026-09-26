package main

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// healthTimeout bounds a single health probe; a backend that doesn't answer
// within it is treated as down.
const healthTimeout = 5 * time.Second

// healthClient dials fast and gives up fast — health probes are lightweight
// reachability checks, not full requests, so they must never hang the poller.
var healthClient = &http.Client{
	Timeout: healthTimeout,
	Transport: func() *http.Transport {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DialContext = (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		return t
	}(),
}

// pingBackend reports whether a backend answers at all. Any HTTP response (even
// 4xx/5xx) counts as reachable; only a connection error or timeout means
// "down". The poller's job is to detect an absent host — a reachable but
// misbehaving one is the circuit breaker's job, handled reactively on request.
func pingBackend(b Backend) bool {
	req, err := http.NewRequest("GET", b.BaseURL+"/", nil)
	if err != nil {
		return false
	}
	resp, err := healthClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// backendHealth holds the last reachability verdict per backend, exposed as a
// gauge so the dashboard can show up/down state without a request touching it.
type backendHealth struct {
	mu sync.Mutex
	m  map[string]bool
}

var health = &backendHealth{m: map[string]bool{}}

func (h *backendHealth) set(name string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.m[name] = ok
}

func (h *backendHealth) state() map[string]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]bool, len(h.m))
	for k, v := range h.m {
		out[k] = v
	}
	return out
}

// startHealthPoller probes every backend immediately and then every interval,
// pre-opening circuits for unreachable backends (so requests fail over
// instantly instead of discovering the outage mid-request) and half-opening
// them on recovery.
func startHealthPoller(cfg Config, interval time.Duration) {
	poll := func() {
		for name, b := range cfg.Backends {
			ok := pingBackend(b)
			health.set(name, ok)
			if ok {
				circuits.markUp(name)
			} else if circuits.markDown(name) {
				metrics.RecordCircuitTrip(name)
			}
		}
	}
	poll()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			poll()
		}
	}()
}
