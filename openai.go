package main

import (
	"encoding/json"
	"net/http"
)

// handleOpenAIModels serves the OpenAI-compatible model list from the
// configured aliases.
func handleOpenAIModels(w http.ResponseWriter, cfg Config) {
	data := make([]map[string]string, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		data = append(data, map[string]string{
			"id":       m.Name,
			"object":   "model",
			"created":  "0",
			"owned_by": "bifrost",
		})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// handleOpenAIChat is a pass-through: it decodes the body as a generic JSON
// object (so client-specific fields survive), remaps the model alias, and
// forwards to the upstream verbatim — preserving streaming SSE or JSON.
func handleOpenAIChat(w http.ResponseWriter, r *http.Request, cfg Config) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	if m, ok := body["model"].(string); ok {
		body["model"] = cfg.upstreamModel(m)
	}
	b, err := json.Marshal(body)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "internal error"})
		return
	}
	if err := forwardRaw(cfg, "/chat/completions", b, w); err != nil {
		// If headers already went out we can't recover, but forwardRaw only
		// returns before writing on a dial/request error.
		writeJSON(w, 502, map[string]string{"error": err.Error()})
	}
}
