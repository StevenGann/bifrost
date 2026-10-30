package main

import (
	"encoding/json"
	"net/http"
	"time"
)

// handleOpenAIModels serves the OpenAI-compatible model list from the
// configured routes, enriched with privacy tier, backend, upstream model, and
// per-1M-token pricing so agents can discover what's available and pick.
func handleOpenAIModels(w http.ResponseWriter, cfg Config) {
	writeJSON(w, 200, map[string]any{"object": "list", "data": cfg.modelCatalog()})
}

// handleOpenAIChat is a pass-through: it decodes the body as a generic JSON
// object (so client-specific fields survive), remaps the model to its routed
// backend + upstream name, and forwards to the backend verbatim — preserving
// streaming SSE or JSON. Transient failures retry and fail over invisibly.
func handleOpenAIChat(w http.ResponseWriter, r *http.Request, cfg Config) {
	app := appFrom(r)
	start := time.Now()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	clientModel, _ := body["model"].(string)
	stream, _ := body["stream"].(bool)

	build := func(up string) []byte {
		if _, ok := body["model"].(string); ok {
			body["model"] = up
		}
		b, _ := json.Marshal(body)
		return b
	}

	var resp *http.Response
	var dialect string // which wire format produced resp
	err, lastUpstream, retries, fallbacks, served := cfg.resolve(clientModel, func(b Backend, up string) (error, bool) {
		if isAnthropic(b) {
			r, e := anthropicAttempt(b, build(up))
			if e == nil {
				resp, dialect = r, "anthropic"
			}
			return e, false
		}
		r, e := rawAttempt(b, "/v1/chat/completions", build(up))
		if e == nil {
			resp, dialect = r, "openai"
		}
		return e, false
	})

	if retries > 0 {
		metrics.RecordRetries(clientModel, app, retries)
	}
	if fallbacks > 0 {
		metrics.RecordFallbacks(clientModel, app, fallbacks)
	}

	if err != nil {
		status := errorStatus(statusOf(err))
		metrics.Record(clientModel, lastUpstream, app, "/v1/chat/completions", status, 0, 0, time.Since(start), true, served)
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}

	defer resp.Body.Close()
	var usage Usage

	if dialect == "anthropic" {
		// Errors arrive as JSON even when the caller asked for a stream, so the
		// status code decides the shape, not the request.
		if stream && resp.StatusCode < 400 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(resp.StatusCode)
			_ = copyAnthropicStream(resp.Body, w, clientModel, &usage)
		} else {
			writeAnthropicResponse(resp, w, clientModel, &usage)
		}
		metrics.Record(clientModel, lastUpstream, app, "/v1/chat/completions", resp.StatusCode, usage.PromptTokens, usage.CompletionTokens, time.Since(start), resp.StatusCode >= 400, served)
		return
	}

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	if stream {
		copyStream(resp.Body, w, &usage)
	} else {
		copyBody(resp.Body, w, &usage)
	}
	metrics.Record(clientModel, lastUpstream, app, "/v1/chat/completions", resp.StatusCode, usage.PromptTokens, usage.CompletionTokens, time.Since(start), resp.StatusCode >= 400, served)
}
