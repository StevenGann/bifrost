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
	pr := defaultPricing()
	out := make([]modelMeta, 0, len(names))
	for _, name := range names {
		route := c.Routes[name]
		backend, ok := c.Backends[route.Backend]
		if !ok {
			backend = Backend{Name: route.Backend}
		}
		out = append(out, modelMeta{
			ID:           name,
			Object:       "model",
			Created:      0,
			OwnedBy:      "bifrost",
			Privacy:      privacyTier(name, backend.Local),
			Backend:      backend.Name,
			Upstream:     route.Upstream,
			Capabilities: capabilities(name),
			Pricing:      pr[route.Upstream],
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
