package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type ollamaEmbeddingsRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaEmbeddingsResponse struct {
	Embedding []float64 `json:"embedding"`
}

type openAIEmbeddingsResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
		Index     int       `json:"index"`
		Object    string    `json:"object"`
	} `json:"data"`
}

// embed requests a single vector from a backend's OpenAI-compatible
// embeddings endpoint (/v1/embeddings).
func embed(b Backend, model, input string) ([]float64, error) {
	body, err := json.Marshal(map[string]any{"model": model, "input": input})
	if err != nil {
		return nil, err
	}
	req, err := upstreamReq(b, "POST", "/v1/embeddings", body)
	if err != nil {
		return nil, err
	}
	resp, err := do(b, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, &upstreamError{resp.StatusCode, fmt.Sprintf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))}
	}
	var out openAIEmbeddingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, &upstreamError{200, err.Error()}
	}
	if len(out.Data) == 0 {
		return nil, &upstreamError{200, "empty embeddings response"}
	}
	return out.Data[0].Embedding, nil
}

// handleEmbeddings serves the Ollama-native embeddings endpoints
// (/api/embeddings, /api/embed), translating to the backend's OpenAI-compatible
// /v1/embeddings and back.
func handleEmbeddings(w http.ResponseWriter, r *http.Request, cfg Config) {
	app := appFrom(r)
	start := time.Now()
	var req ollamaEmbeddingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}

	var result []float64
	err, lastUpstream, retries, fallbacks := cfg.resolve(req.Model, func(b Backend, up string) (error, bool) {
		emb, e := embed(b, up, req.Prompt)
		if e == nil {
			result = emb
		}
		return e, false
	})
	if retries > 0 {
		metrics.RecordRetries(req.Model, app, retries)
	}
	if fallbacks > 0 {
		metrics.RecordFallbacks(req.Model, app, fallbacks)
	}
	if err != nil {
		status := errorStatus(statusOf(err))
		metrics.Record(req.Model, lastUpstream, app, "/api/embeddings", status, 0, 0, time.Since(start), true)
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	metrics.Record(req.Model, lastUpstream, app, "/api/embeddings", 200, 0, 0, time.Since(start), false)
	writeJSON(w, 200, ollamaEmbeddingsResponse{Embedding: result})
}

// handleOpenAIEmbeddings passes the OpenAI-compatible /v1/embeddings through to
// the routed backend, remapping the model name.
func handleOpenAIEmbeddings(w http.ResponseWriter, r *http.Request, cfg Config) {
	app := appFrom(r)
	start := time.Now()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	clientModel, _ := body["model"].(string)

	build := func(up string) []byte {
		if _, ok := body["model"].(string); ok {
			body["model"] = up
		}
		b, _ := json.Marshal(body)
		return b
	}

	var resp *http.Response
	err, lastUpstream, retries, fallbacks := cfg.resolve(clientModel, func(b Backend, up string) (error, bool) {
		r, e := rawAttempt(b, "/v1/embeddings", build(up))
		if e == nil {
			resp = r
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
		metrics.Record(clientModel, lastUpstream, app, "/v1/embeddings", status, 0, 0, time.Since(start), true)
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}

	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	var usage Usage
	copyBody(resp.Body, w, &usage)
	metrics.Record(clientModel, lastUpstream, app, "/v1/embeddings", resp.StatusCode, 0, 0, time.Since(start), resp.StatusCode >= 400)
}
