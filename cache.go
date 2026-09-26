package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// cacheEntry is one cached response with its expiry and last-access time.
type cacheEntry struct {
	value      []byte
	expires    time.Time
	lastAccess time.Time
}

// cache is a small, in-memory, TTL-bounded LRU of opaque response bytes. It is
// deliberately never persisted: cached responses can echo request PII, so they
// stay in process memory only and die with the process.
type cache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	max     int
	ttl     time.Duration
}

func newCache(max int, ttl time.Duration) *cache {
	return &cache{entries: map[string]*cacheEntry{}, max: max, ttl: ttl}
}

// newCacheFromConfig builds a cache from integer env values (seconds, entries).
func newCacheFromConfig(ttlSec, max int) *cache {
	return newCache(max, time.Duration(ttlSec)*time.Second)
}

// enabled reports whether caching is active.
func (c *cache) enabled() bool { return c != nil && c.ttl > 0 && c.max > 0 }

func (c *cache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expires) {
		delete(c.entries, key)
		return nil, false
	}
	e.lastAccess = time.Now()
	return e.value, true
}

func (c *cache) Set(key string, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = &cacheEntry{value: value, expires: time.Now().Add(c.ttl), lastAccess: time.Now()}
	if len(c.entries) > c.max {
		c.evictOldest()
	}
}

func (c *cache) evictOldest() {
	var oldestKey string
	var oldest time.Time
	for k, e := range c.entries {
		if oldestKey == "" || e.lastAccess.Before(oldest) {
			oldestKey = k
			oldest = e.lastAccess
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// cacheKey derives a stable cache key from an endpoint and a request struct.
// json.Marshal of a struct is deterministic, so identical logical requests
// (regardless of field order or whitespace) map to the same key.
func cacheKey(endpoint string, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(append([]byte(endpoint+"\x00"), b...))
	return hex.EncodeToString(h[:])
}

// lruCache is the package-level response cache, a sensible default here,
// replaced by main() with the env-configured instance.
var lruCache = newCache(256, 300*time.Second)

// completeCached wraps complete with an exact-match response cache plus an
// opt-in semantic cache. An exact hit returns the cached OpenAI response
// instantly (no upstream call, no cost). On an exact miss, if semantic caching
// is enabled the prompt is embedded and a near-neighbor (cosine-similar) cached
// response is served; otherwise complete is called and the result cached both
// ways. Cache hits/misses are recorded in metrics.
func (c Config) completeCached(key string, clientModel, app, endpoint string, build func(string) OpenAIRequest) (*OpenAIChatResponse, Usage, int, error) {
	if key != "" && lruCache.enabled() {
		if cached, ok := lruCache.Get(key); ok {
			var resp OpenAIChatResponse
			if json.Unmarshal(cached, &resp) == nil {
				metrics.RecordCacheHit(clientModel, app)
				return &resp, Usage{}, 200, nil
			}
		}
	}

	// Semantic lookup: embed the prompt once and reuse the vector to store on
	// miss. A failed embed (e.g. embedding backend down) degrades to no
	// semantic caching — the request still proceeds normally.
	var queryEmb []float64
	if semanticCache.enabled() {
		if emb, err := c.embedQuery(queryText(build(""))); err == nil {
			queryEmb = emb
			if cached, ok := semanticCache.get(clientModel, emb); ok {
				var resp OpenAIChatResponse
				if json.Unmarshal(cached, &resp) == nil {
					metrics.RecordSemanticHit(clientModel, app)
					return &resp, Usage{}, 200, nil
				}
			}
			metrics.RecordSemanticMiss(clientModel, app)
		}
	}

	resp, usage, status, err := c.complete(clientModel, app, endpoint, build)
	if err != nil {
		return resp, usage, status, err
	}

	var cached []byte
	if (key != "" && lruCache.enabled()) || queryEmb != nil {
		cached, _ = json.Marshal(resp)
	}
	if cached != nil && key != "" && lruCache.enabled() {
		lruCache.Set(key, cached)
		metrics.RecordCacheMiss(clientModel, app)
	}
	if cached != nil && queryEmb != nil {
		semanticCache.set(clientModel, queryEmb, cached)
	}
	return resp, usage, status, nil
}
