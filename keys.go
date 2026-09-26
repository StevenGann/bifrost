package main

import (
	"sync"
	"time"
)

// keyCooldown is how long a rate-limited or auth-rejected key is skipped before
// it becomes eligible again. Mirrors the circuit breaker's cooldown window.
const keyCooldown = 30 * time.Second

// apiKey is one credential in a backend's key pool.
type apiKey struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Plan   string `json:"plan"`
	Weight int    `json:"weight"`
}

// keyRing rotates across a backend's keys, skipping keys that are cooling down
// after a rate-limit/auth failure.
type keyRing struct {
	mu       sync.Mutex
	keys     []apiKey
	pos      int
	cooldown map[string]time.Time
	d        time.Duration
}

func newKeyRing(keys []apiKey, d time.Duration) *keyRing {
	return &keyRing{keys: keys, cooldown: map[string]time.Time{}, d: d}
}

// pick returns the next non-cooling key (weighted round-robin). When every key
// is cooling down it returns the one expiring soonest, so a request still has a
// chance rather than stalling.
func (k *keyRing) pick() (apiKey, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.keys) == 0 {
		return apiKey{}, false
	}
	now := time.Now()
	var slots []int
	for i, key := range k.keys {
		w := key.Weight
		if w <= 0 {
			w = 1
		}
		for j := 0; j < w; j++ {
			slots = append(slots, i)
		}
	}
	for step := 0; step < len(slots); step++ {
		idx := slots[k.pos%len(slots)]
		k.pos = (k.pos + 1) % len(slots)
		if until, cooling := k.cooldown[k.keys[idx].ID]; cooling && until.After(now) {
			continue
		}
		return k.keys[idx], true
	}
	var best apiKey
	var bestUntil time.Time
	for i, key := range k.keys {
		until := k.cooldown[key.ID]
		if i == 0 || until.Before(bestUntil) {
			best, bestUntil = key, until
		}
	}
	return best, true
}

func (k *keyRing) markCooling(id string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.cooldown[id] = time.Now().Add(k.d)
}

// keySet is the process-wide registry of per-backend key rings (like circuits).
type keySet struct {
	mu sync.Mutex
	m  map[string]*keyRing
	d  time.Duration
}

func newKeySet(d time.Duration) *keySet { return &keySet{m: map[string]*keyRing{}, d: d} }

func (ks *keySet) pick(name string, keys []apiKey) (apiKey, bool) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	r, ok := ks.m[name]
	if !ok {
		r = newKeyRing(keys, ks.d)
		ks.m[name] = r
	}
	return r.pick()
}

func (ks *keySet) markCooling(name, id string) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if r, ok := ks.m[name]; ok {
		r.markCooling(id)
	}
}

// keyRings is the shared registry, replaced by tests.
var keyRings = newKeySet(keyCooldown)

// keyRateLimited reports whether an error is the kind that should cool a key:
// rate-limit (429) or an auth rejection (401/403). Server errors (5xx) and
// transport failures are backend problems, not key problems.
func keyRateLimited(err error) bool {
	ue, ok := err.(*upstreamError)
	if !ok {
		return false
	}
	return ue.status == 429 || ue.status == 401 || ue.status == 403
}
