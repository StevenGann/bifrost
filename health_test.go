package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPingBackend(t *testing.T) {
	// A 404 is still a response — the host is reachable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()
	if !pingBackend(Backend{Name: "up", BaseURL: srv.URL}) {
		t.Fatal("expected reachable backend (any HTTP response counts)")
	}
	if pingBackend(Backend{Name: "down", BaseURL: "http://127.0.0.1:1"}) {
		t.Fatal("expected unreachable backend")
	}
}

func TestMarkDownOpensCircuit(t *testing.T) {
	c := &Circuit{threshold: 3, cooldown: time.Hour}
	if !c.allow() {
		t.Fatal("closed circuit should allow")
	}
	if !c.markDown() {
		t.Fatal("markDown should report a new open")
	}
	if c.allow() {
		t.Fatal("opened circuit should deny")
	}
}

func TestMarkDownRefreshesWindow(t *testing.T) {
	c := &Circuit{threshold: 3, cooldown: time.Hour}
	c.markDown()
	if c.markDown() {
		t.Fatal("markDown on an already-open circuit should not report a new open")
	}
}

func TestMarkUpHalfOpens(t *testing.T) {
	c := &Circuit{threshold: 3, cooldown: time.Hour}
	c.markDown()
	if c.allow() {
		t.Fatal("opened circuit should deny before markUp")
	}
	c.markUp()
	if !c.allow() {
		t.Fatal("half-opened circuit should admit a probe")
	}
}

func TestMarkUpProbeSuccessCloses(t *testing.T) {
	c := &Circuit{threshold: 3, cooldown: time.Hour}
	c.markDown()
	c.markUp()
	c.recordSuccess()
	if c.open() {
		t.Fatal("circuit should close after the probe succeeds")
	}
}

func TestMarkUpProbeFailureReopens(t *testing.T) {
	c := &Circuit{threshold: 3, cooldown: time.Hour}
	c.markDown()
	c.markUp()
	c.recordFailure()
	if !c.open() {
		t.Fatal("circuit should re-open after the probe fails")
	}
}

func TestMarkUpNoOpWhenClosed(t *testing.T) {
	c := &Circuit{threshold: 3, cooldown: time.Hour}
	c.recordFailure() // failures = 1, still closed
	c.markUp()
	if c.failures != 1 {
		t.Fatalf("markUp should not touch a closed circuit, failures=%d", c.failures)
	}
	if !c.allow() {
		t.Fatal("still-closed circuit should allow")
	}
}

func TestHealthPollerImmediatePoll(t *testing.T) {
	oldCircuits, oldHealth, oldMetrics := circuits, health, metrics
	circuits = newCircuits(3, time.Hour)
	health = &backendHealth{m: map[string]bool{}}
	metrics = newMetrics(defaultPricing())
	defer func() { circuits, health, metrics = oldCircuits, oldHealth, oldMetrics }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := Config{Backends: map[string]Backend{
		"up":   {Name: "up", BaseURL: srv.URL},
		"down": {Name: "down", BaseURL: "http://127.0.0.1:1"},
	}}
	// Long interval so the background ticker never re-fires during the test.
	startHealthPoller(cfg, time.Hour)

	if !health.state()["up"] {
		t.Error("reachable backend should be marked healthy")
	}
	if health.state()["down"] {
		t.Error("unreachable backend should be marked unhealthy")
	}
	if !circuits.state()["down"] {
		t.Error("unreachable backend's circuit should be pre-opened")
	}
	if circuits.state()["up"] {
		t.Error("reachable backend's circuit should stay closed")
	}
	if metrics.circuitTrips["down"] != 1 {
		t.Errorf("proactive open should count as one trip, got %d", metrics.circuitTrips["down"])
	}
}
