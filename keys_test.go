package main

import (
	"testing"
	"time"
)

func TestKeyRingRoundRobin(t *testing.T) {
	r := newKeyRing([]apiKey{{ID: "a", Key: "ka"}, {ID: "b", Key: "kb"}}, time.Minute)
	var ids []string
	for i := 0; i < 3; i++ {
		k, _ := r.pick()
		ids = append(ids, k.ID)
	}
	if ids[0] != "a" || ids[1] != "b" || ids[2] != "a" {
		t.Errorf("round-robin = %v, want [a b a]", ids)
	}
}

func TestKeyRingWeight(t *testing.T) {
	// weight 3 on "a", 1 on "b" → the first four picks are a a a b.
	r := newKeyRing([]apiKey{{ID: "a", Key: "ka", Weight: 3}, {ID: "b", Key: "kb", Weight: 1}}, time.Minute)
	var ids []string
	for i := 0; i < 4; i++ {
		k, _ := r.pick()
		ids = append(ids, k.ID)
	}
	want := []string{"a", "a", "a", "b"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("weighted = %v, want %v", ids, want)
		}
	}
}

func TestKeyRingCooldown(t *testing.T) {
	r := newKeyRing([]apiKey{{ID: "a", Key: "ka"}, {ID: "b", Key: "kb"}}, time.Minute)
	r.markCooling("a")
	for i := 0; i < 2; i++ {
		k, _ := r.pick()
		if k.ID != "b" {
			t.Errorf("pick %d = %q, want b (a is cooling)", i, k.ID)
		}
	}
}

func TestKeyRateLimited(t *testing.T) {
	if !keyRateLimited(&upstreamError{429, "x"}) {
		t.Error("429 should be keyRateLimited")
	}
	if !keyRateLimited(&upstreamError{401, "x"}) {
		t.Error("401 should be keyRateLimited")
	}
	if !keyRateLimited(&upstreamError{403, "x"}) {
		t.Error("403 should be keyRateLimited")
	}
	if keyRateLimited(&upstreamError{500, "x"}) {
		t.Error("500 should NOT be keyRateLimited")
	}
	if keyRateLimited(&upstreamError{0, "x"}) {
		t.Error("transport failure should NOT be keyRateLimited")
	}
}

func TestResolveKeyRotation(t *testing.T) {
	old := keyRings
	keyRings = newKeySet(time.Minute)
	defer func() { keyRings = old }()

	cfg := Config{
		Backends: map[string]Backend{
			"test": {Name: "test", BaseURL: "http://x", Keys: []apiKey{
				{ID: "a", Key: "key-a"}, {ID: "b", Key: "key-b"},
			}},
		},
		Routes:  map[string]Route{"m": {Name: "m", Backend: "test", Upstream: "up"}},
		Retries: 2,
	}

	attempts := 0
	var keysUsed []string
	_, up, retries, fallbacks := cfg.resolve("m", func(b Backend, up string) (error, bool) {
		attempts++
		keysUsed = append(keysUsed, b.APIKey)
		if b.APIKey == "key-a" {
			return &upstreamError{429, "rate limited"}, false
		}
		return nil, false
	})
	if up != "up" || attempts != 2 || retries != 1 || fallbacks != 0 {
		t.Fatalf("up=%q attempts=%d retries=%d fallbacks=%d", up, attempts, retries, fallbacks)
	}
	if keysUsed[0] != "key-a" || keysUsed[1] != "key-b" {
		t.Errorf("keys used = %v, want [key-a key-b]", keysUsed)
	}
}
