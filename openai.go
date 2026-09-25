package main

import (
	"encoding/json"
	"net/http"
	"time"
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
	app := appFrom(r)
	start := time.Now()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	clientModel, _ := body["model"].(string)
	upstreamModel := cfg.upstreamModel(clientModel)
	if _, ok := body["model"].(string); ok {
		body["model"] = upstreamModel
	}
	b, err := json.Marshal(body)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "internal error"})
		return
	}

	var usage Usage
	status, err := forwardRaw(cfg, "/chat/completions", b, w, &usage)
	if err != nil && status == 0 {
		status = 502
		writeJSON(w, 502, map[string]string{"error": err.Error()})
	}
	isErr := err != nil || status >= 400
	metrics.Record(clientModel, upstreamModel, app, "/v1/chat/completions", status, usage.PromptTokens, usage.CompletionTokens, time.Since(start), isErr)
}
