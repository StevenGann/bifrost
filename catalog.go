package main

import (
	"net/http"
	"strings"
)

// modelMeta describes a client-facing model for the catalog endpoints.
// Extra fields beyond the OpenAI baseline are additive — standard clients
// ignore what they don't recognize, while agents read privacy/backend/pricing.
type modelMeta struct {
	ID           string   `json:"id"`
	Object       string   `json:"object"`
	Created      int      `json:"created"`
	OwnedBy      string   `json:"owned_by"`
	Privacy      string   `json:"privacy"` // "private" | "local" | "cloud"
	Backend      string   `json:"backend"`
	Upstream     string   `json:"upstream"`
	Capabilities []string `json:"capabilities"`
	Pricing      pricing  `json:"pricing"` // USD per 1M tokens (cache-miss)
	// ContextLength is the model's real prompt window. Clients that probe for
	// it and get nothing fall back to a guess — Hermes guessed 1,000,000 for
	// claude-opus-5-5 (a substring match on its built-in table) and never
	// compacted before Anthropic's 200k wall, so every long session ended in a
	// context-length 400. Advertising the truth is the fix.
	ContextLength int `json:"context_length,omitempty"`
}

// defaultContextLength is the window for a routed model, keyed by upstream
// name prefix. Only entries that have been verified belong here; an unknown
// model advertises nothing, and the client falls back to its own default.
func defaultContextLength(upstream string) int {
	switch {
	case strings.HasPrefix(upstream, "claude"):
		return 200_000
	}
	return 0
}

// privacyTier classifies a model: private:* is always private; otherwise local
// backends are "local" and everything else is "cloud".
func privacyTier(name string, local bool) string {
	if strings.HasPrefix(name, "private:") {
		return "private"
	}
	if local {
		return "local"
	}
	return "cloud"
}

// capabilities derives a model's capabilities from its name (a proxy for the
// embedding convention; "embed" in the name ⇒ embeddings, else chat).
func capabilities(name string) []string {
	if strings.Contains(name, "embed") {
		return []string{"embeddings"}
	}
	return []string{"chat", "completions"}
}

// modelCatalog returns metadata for every client-facing model, sorted by name.
func (c Config) modelCatalog() []modelMeta {
	names := c.modelNames()
	// loadPricing, not defaultPricing: the catalog must advertise the same
	// numbers the ledger charges. Anything added through PRICING (Claude,
	// local rates) was invisible here and showed up as free.
	pr := loadPricing()
	out := make([]modelMeta, 0, len(names))
	for _, name := range names {
		route := c.Routes[name]
		backend, ok := c.Backends[route.Backend]
		if !ok {
			backend = Backend{Name: route.Backend}
		}
		out = append(out, modelMeta{
			ID:            name,
			Object:        "model",
			Created:       0,
			OwnedBy:       "bifrost",
			Privacy:       privacyTier(name, backend.Local),
			Backend:       backend.Name,
			Upstream:      route.Upstream,
			Capabilities:  capabilities(name),
			Pricing:       pr[route.Upstream],
			ContextLength: defaultContextLength(route.Upstream),
		})
	}
	return out
}

// handleOpenAIModelDetail serves a single model's metadata (OpenAI shape).
func handleOpenAIModelDetail(w http.ResponseWriter, r *http.Request, cfg Config) {
	name := r.PathValue("name")
	for _, m := range cfg.modelCatalog() {
		if m.ID == name {
			writeJSON(w, 200, m)
			return
		}
	}
	writeJSON(w, 404, map[string]string{"error": "model not found"})
}
