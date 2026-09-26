package main

import (
	"errors"
	"testing"
)

func TestResolveRetriesTransient(t *testing.T) {
	cfg := Config{
		Backends: map[string]Backend{"b": {Name: "b", BaseURL: "http://x", Local: false}},
		Default:  "b",
		Routes:   map[string]Route{"m": {Name: "m", Backend: "b", Upstream: "up"}},
		Retries:  2,
	}
	calls := 0
	err, up, retries, fb := cfg.resolve("m", func(b Backend, u string) (error, bool) {
		calls++
		if calls < 2 {
			return &upstreamError{500, "boom"}, false
		}
		return nil, false
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if calls != 2 || retries != 1 || fb != 0 || up != "up" {
		t.Fatalf("calls=%d retries=%d fb=%d up=%q", calls, retries, fb, up)
	}
}

func TestResolveStreamingRetryBeforeCommit(t *testing.T) {
	cfg := Config{
		Backends: map[string]Backend{"b": {Name: "b", BaseURL: "http://x", Local: false}},
		Default:  "b",
		Routes:   map[string]Route{"m": {Name: "m", Backend: "b", Upstream: "up"}},
		Retries:  2,
	}
	calls := 0
	err, _, retries, _ := cfg.resolve("m", func(b Backend, u string) (error, bool) {
		calls++
		if calls == 1 {
			return &upstreamError{0, "conn refused"}, false // pre-commit failure
		}
		return nil, true // committed success
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if calls != 2 || retries != 1 {
		t.Fatalf("calls=%d retries=%d", calls, retries)
	}
}

func TestResolveFailsOver(t *testing.T) {
	cfg := Config{
		Backends: map[string]Backend{
			"a": {Name: "a", BaseURL: "http://a", Local: false},
			"b": {Name: "b", BaseURL: "http://b", Local: false},
		},
		Default: "a",
		Routes: map[string]Route{
			"m":  {Name: "m", Backend: "a", Upstream: "up-a"},
			"m2": {Name: "m2", Backend: "b", Upstream: "up-b"},
		},
		Fallbacks: map[string]string{"m": "m2"},
	}
	var lastUp string
	err, up, _, fb := cfg.resolve("m", func(b Backend, u string) (error, bool) {
		lastUp = u
		if b.Name == "a" {
			return &upstreamError{503, "down"}, false
		}
		return nil, false
	})
	if err != nil {
		t.Fatalf("expected success via fallback, got %v", err)
	}
	if fb != 1 || lastUp != "up-b" || up != "up-b" {
		t.Fatalf("fb=%d lastUp=%q up=%q", fb, lastUp, up)
	}
}

func TestResolveCircuitOpenSkipsRetryFailsOver(t *testing.T) {
	cfg := Config{
		Backends: map[string]Backend{
			"a": {Name: "a", BaseURL: "http://a", Local: false},
			"b": {Name: "b", BaseURL: "http://b", Local: false},
		},
		Default: "a",
		Routes: map[string]Route{
			"m":  {Name: "m", Backend: "a", Upstream: "up-a"},
			"m2": {Name: "m2", Backend: "b", Upstream: "up-b"},
		},
		Fallbacks: map[string]string{"m": "m2"},
		Retries:   5,
	}
	callsA := 0
	err, _, retries, fb := cfg.resolve("m", func(b Backend, u string) (error, bool) {
		if b.Name == "a" {
			callsA++
			return errCircuitOpen, false
		}
		return nil, false
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if retries != 0 || fb != 1 || callsA != 1 {
		t.Fatalf("retries=%d fb=%d callsA=%d (want 0/1/1)", retries, fb, callsA)
	}
}

func TestResolveCommittedStopsRetry(t *testing.T) {
	cfg := Config{
		Backends: map[string]Backend{"b": {Name: "b", BaseURL: "http://x", Local: false}},
		Default:  "b",
		Routes:   map[string]Route{"m": {Name: "m", Backend: "b", Upstream: "up"}},
		Retries:  5,
	}
	calls := 0
	err, _, retries, fb := cfg.resolve("m", func(b Backend, u string) (error, bool) {
		calls++
		return errors.New("mid-stream"), true // committed
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 || retries != 0 || fb != 0 {
		t.Fatalf("calls=%d retries=%d fb=%d (want 1/0/0)", calls, retries, fb)
	}
}
