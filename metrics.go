package main

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pricing is USD per 1M tokens (cache-miss rate). Peak hours double it.
type pricing struct {
	InputPer1M  float64 `json:"input"`
	OutputPer1M float64 `json:"output"`
}

// defaultPricing returns DeepSeek v4 official cache-miss rates (USD / 1M tokens),
// read off DeepSeek's pricing page 2026-08-18. Local/unknown models cost $0.
func defaultPricing() map[string]pricing {
	return map[string]pricing{
		"deepseek-v4-flash": {InputPer1M: 0.22, OutputPer1M: 0.66},
		"deepseek-v4-pro":   {InputPer1M: 0.66, OutputPer1M: 1.98},
	}
}

// isPeak reports whether t falls in DeepSeek's peak window: Monday–Friday,
// 01:00–04:00 and 06:00–10:00 UTC. Off-peak rates are exactly half of peak.
func isPeak(t time.Time) bool {
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return false
	}
	h := t.UTC().Hour()
	return (h >= 1 && h < 4) || (h >= 6 && h < 10)
}

var latencyBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

type histogram struct {
	counts []uint64 // cumulative: counts[i] = observations <= latencyBuckets[i]
	sum    float64
	count  uint64
}

func (h *histogram) observe(seconds float64) {
	for i, b := range latencyBuckets {
		if seconds <= b {
			h.counts[i]++
		}
	}
	h.sum += seconds
	h.count++
}

// Metrics collects per-request counters for completion traffic and renders them
// in Prometheus text format. Concurrency-safe.
type Metrics struct {
	mu      sync.Mutex
	pricing map[string]pricing

	requests  map[string]uint64     // "model|app|endpoint|status"
	errors    map[string]uint64     // "model|app|endpoint"
	tokensIn  map[string]uint64     // "model|app"
	tokensOut map[string]uint64     // "model|app"
	costUSD   map[string]float64    // "model|app"
	latency   map[string]*histogram // "model|app"
}

func newMetrics(p map[string]pricing) *Metrics {
	return &Metrics{
		pricing:   p,
		requests:  map[string]uint64{},
		errors:    map[string]uint64{},
		tokensIn:  map[string]uint64{},
		tokensOut: map[string]uint64{},
		costUSD:   map[string]float64{},
		latency:   map[string]*histogram{},
	}
}

// Record a completed completion request. clientModel is the caller-facing name
// used as the metric label; upstreamModel is what cost is priced against.
func (m *Metrics) Record(clientModel, upstreamModel, app, endpoint string, status int, tokIn, tokOut int, dur time.Duration, isErr bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ma := clientModel + "|" + app
	m.requests[ma+"|"+endpoint+"|"+strconv.Itoa(status)]++
	m.tokensIn[ma] += uint64(tokIn)
	m.tokensOut[ma] += uint64(tokOut)
	m.costUSD[ma] += m.cost(upstreamModel, tokIn, tokOut)
	if isErr {
		m.errors[ma+"|"+endpoint]++
	}
	h := m.latency[ma]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(latencyBuckets))}
		m.latency[ma] = h
	}
	h.observe(dur.Seconds())
}

func (m *Metrics) cost(upstreamModel string, tokIn, tokOut int) float64 {
	return m.costAt(upstreamModel, tokIn, tokOut, time.Now())
}

func (m *Metrics) costAt(upstreamModel string, tokIn, tokOut int, at time.Time) float64 {
	p, ok := m.pricing[upstreamModel]
	if !ok {
		return 0
	}
	in, out := p.InputPer1M, p.OutputPer1M
	if isPeak(at) {
		in *= 2
		out *= 2
	}
	return float64(tokIn)*in/1e6 + float64(tokOut)*out/1e6
}

// Handler serves the Prometheus text exposition for the scrape endpoint.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		m.writeLocked(w)
	})
}

func (m *Metrics) writeLocked(w io.Writer) {
	reqKeys := sortedKeys(m.requests)
	errKeys := sortedKeys(m.errors)

	maSet := map[string]bool{}
	for k := range m.tokensIn {
		maSet[k] = true
	}
	for k := range m.latency {
		maSet[k] = true
	}
	maKeys := make([]string, 0, len(maSet))
	for k := range maSet {
		maKeys = append(maKeys, k)
	}
	sort.Strings(maKeys)

	fmt.Fprintf(w, "# HELP bifrost_requests_total Total completion requests.\n")
	fmt.Fprintf(w, "# TYPE bifrost_requests_total counter\n")
	for _, k := range reqKeys {
		p := strings.Split(k, "|")
		fmt.Fprintf(w, "bifrost_requests_total{model=%q,app=%q,endpoint=%q,status=%q} %d\n",
			p[0], p[1], p[2], p[3], m.requests[k])
	}

	fmt.Fprintf(w, "# HELP bifrost_tokens_total Total tokens, by direction.\n")
	fmt.Fprintf(w, "# TYPE bifrost_tokens_total counter\n")
	for _, k := range maKeys {
		p := strings.SplitN(k, "|", 2)
		fmt.Fprintf(w, "bifrost_tokens_total{model=%q,app=%q,direction=\"input\"} %d\n", p[0], p[1], m.tokensIn[k])
		fmt.Fprintf(w, "bifrost_tokens_total{model=%q,app=%q,direction=\"output\"} %d\n", p[0], p[1], m.tokensOut[k])
	}

	fmt.Fprintf(w, "# HELP bifrost_cost_usd_total Estimated cost (cache-miss rates, peak/off-peak aware).\n")
	fmt.Fprintf(w, "# TYPE bifrost_cost_usd_total counter\n")
	for _, k := range maKeys {
		p := strings.SplitN(k, "|", 2)
		fmt.Fprintf(w, "bifrost_cost_usd_total{model=%q,app=%q} %s\n", p[0], p[1], f64(m.costUSD[k]))
	}

	fmt.Fprintf(w, "# HELP bifrost_errors_total Upstream/completion errors.\n")
	fmt.Fprintf(w, "# TYPE bifrost_errors_total counter\n")
	for _, k := range errKeys {
		p := strings.Split(k, "|")
		fmt.Fprintf(w, "bifrost_errors_total{model=%q,app=%q,endpoint=%q} %d\n", p[0], p[1], p[2], m.errors[k])
	}

	fmt.Fprintf(w, "# HELP bifrost_request_duration_seconds Completion request latency.\n")
	fmt.Fprintf(w, "# TYPE bifrost_request_duration_seconds histogram\n")
	for _, k := range maKeys {
		p := strings.SplitN(k, "|", 2)
		h := m.latency[k]
		if h == nil {
			continue
		}
		for i, b := range latencyBuckets {
			fmt.Fprintf(w, "bifrost_request_duration_seconds_bucket{model=%q,app=%q,le=%q} %d\n", p[0], p[1], f64(b), h.counts[i])
		}
		fmt.Fprintf(w, "bifrost_request_duration_seconds_bucket{model=%q,app=%q,le=\"+Inf\"} %d\n", p[0], p[1], h.count)
		fmt.Fprintf(w, "bifrost_request_duration_seconds_sum{model=%q,app=%q} %s\n", p[0], p[1], f64(h.sum))
		fmt.Fprintf(w, "bifrost_request_duration_seconds_count{model=%q,app=%q} %d\n", p[0], p[1], h.count)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func f64(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
