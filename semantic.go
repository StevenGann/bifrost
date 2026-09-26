package main

import (
	"errors"
	"math"
	"strings"
	"sync"
	"time"
)

// semanticEntry is one cached response plus the embedding of the prompt that
// produced it, used for near-neighbor (semantic) lookups.
type semanticEntry struct {
	embedding  []float64
	value      []byte
	expires    time.Time
	lastAccess time.Time
}

// semanticCache serves a response for a *similar* prompt (cosine similarity at
// or above a threshold) rather than only exact matches. In-memory only, like
// the exact-match cache; keyed per client model so two models never share an
// answer for the same query.
type semanticStore struct {
	mu        sync.Mutex
	byModel   map[string][]*semanticEntry
	max       int
	ttl       time.Duration
	threshold float64
}

func newSemanticCache(max int, ttl time.Duration, threshold float64) *semanticStore {
	return &semanticStore{byModel: map[string][]*semanticEntry{}, max: max, ttl: ttl, threshold: threshold}
}

func (sc *semanticStore) enabled() bool { return sc != nil && sc.ttl > 0 && sc.max > 0 }

// get returns the cached value of the nearest (above-threshold) neighbor to q
// within model's entries, or (nil, false).
func (sc *semanticStore) get(model string, q []float64) ([]byte, bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	now := time.Now()
	best := -1.0
	var bestEntry *semanticEntry
	for _, e := range sc.byModel[model] {
		if now.After(e.expires) {
			continue
		}
		if s := cosine(q, e.embedding); s >= sc.threshold && s > best {
			best = s
			bestEntry = e
		}
	}
	if bestEntry == nil {
		return nil, false
	}
	bestEntry.lastAccess = now
	return bestEntry.value, true
}

func (sc *semanticStore) set(model string, emb []float64, value []byte) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.byModel[model] = append(sc.byModel[model], &semanticEntry{
		embedding: emb, value: value,
		expires: time.Now().Add(sc.ttl), lastAccess: time.Now(),
	})
	total := 0
	for _, entries := range sc.byModel {
		total += len(entries)
	}
	for total > sc.max {
		sc.evictOldestLocked()
		total--
	}
}

// evictOldestLocked removes the least-recently-accessed entry across all
// models. The caller must hold sc.mu.
func (sc *semanticStore) evictOldestLocked() {
	var oldestModel string
	oldestIdx := -1
	var oldest time.Time
	for m, entries := range sc.byModel {
		for i, e := range entries {
			if oldestIdx == -1 || e.lastAccess.Before(oldest) {
				oldestModel, oldestIdx, oldest = m, i, e.lastAccess
			}
		}
	}
	if oldestIdx >= 0 {
		entries := sc.byModel[oldestModel]
		sc.byModel[oldestModel] = append(entries[:oldestIdx], entries[oldestIdx+1:]...)
	}
}

// cosine returns the cosine similarity of two equal-length vectors.
func cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// queryText flattens a conversation into the string that gets embedded for a
// semantic lookup: the whole exchange, not just the last user turn, so a
// similar question in a different context does not collide.
func queryText(req OpenAIRequest) string {
	if len(req.Messages) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, m := range req.Messages {
		sb.WriteString(m.Role)
		sb.WriteString(": ")
		sb.WriteString(m.Content)
		sb.WriteByte('\n')
	}
	return strings.TrimSpace(sb.String())
}

// embedQuery embeds a query string using the configured embedding route,
// single-attempt (no retry): semantic caching is a best-effort optimization,
// and a downed embedding backend should not add latency to every request.
func (c Config) embedQuery(text string) ([]float64, error) {
	if text == "" {
		return nil, errors.New("empty query")
	}
	b, up, err := c.route(c.EmbedModel)
	if err != nil {
		return nil, err
	}
	return embed(b, up, text)
}

// semanticCache is the package-level semantic cache; disabled by default and
// replaced by main() when SEMANTIC_CACHE is enabled.
var semanticCache = newSemanticCache(0, 0, 0.92)
