package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCircuitOpensAfterThreshold(t *testing.T) {
	c := &Circuit{threshold: 2, cooldown: time.Hour}
	if !c.allow() {
		t.Fatal("should allow below threshold")
	}
	if c.recordFailure() {
		t.Fatal("first failure should not open a threshold-2 circuit")
	}
	if !c.allow() {
		t.Fatal("should still allow below threshold")
	}
	if !c.recordFailure() {
		t.Fatal("second failure should open the circuit")
	}
	if c.allow() {
		t.Fatal("should not allow once open")
	}
}

func TestCircuitHalfOpenProbe(t *testing.T) {
	c := &Circuit{threshold: 1, cooldown: 50 * time.Millisecond}
	c.recordFailure() // opens immediately (threshold 1)
	if c.allow() {
		t.Fatal("should not allow immediately after opening")
	}
	time.Sleep(60 * time.Millisecond)
	if !c.allow() {
		t.Fatal("should allow a probe after cooldown")
	}
	if c.allow() {
		t.Fatal("probe is single-shot: window resets, second call should be denied")
	}
}

func TestCircuitRecordSuccessCloses(t *testing.T) {
	c := &Circuit{threshold: 1, cooldown: time.Hour}
	c.recordFailure()
	if !c.open() {
		t.Fatal("should be open after threshold failure")
	}
	c.recordSuccess()
	if c.open() {
		t.Fatal("should close on success")
	}
	if c.failures != 0 {
		t.Fatal("failures should reset on success")
	}
}

func TestDoFailsFastWhenCircuitOpen(t *testing.T) {
	old := circuits
	circuits = newCircuits(1, time.Hour)
	defer func() { circuits = old }()

	called := false
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer mock.Close()

	b := Backend{Name: "circuit-test", BaseURL: mock.URL}
	req, _ := http.NewRequest("POST", mock.URL+"/v1/chat/completions", nil)

	if !circuits.recordFailure("circuit-test") {
		t.Fatal("expected first failure to open a threshold-1 circuit")
	}

	_, err := do(b, req)
	if err == nil {
		t.Fatal("expected circuit-open error")
	}
	if !errors.Is(err, errCircuitOpen) {
		t.Fatalf("expected errCircuitOpen, got %T %v", err, err)
	}
	if called {
		t.Fatal("server must not be called when the circuit is open")
	}
}
