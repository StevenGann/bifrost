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
	backend, upstream, rerr := cfg.route(req.Model)
	if rerr != nil {
		metrics.Record(req.Model, req.Model, app, "/api/embeddings", 400, 0, 0, time.Since(start), true)
		writeJSON(w, 400, map[string]string{"error": rerr.Error()})
		return
	}
	emb, err := embed(backend, upstream, req.Prompt)
	if err != nil {
		status := 502
		if ue, ok := err.(*upstreamError); ok && ue.status >= 400 {
			status = ue.status
		}
		metrics.Record(req.Model, upstream, app, "/api/embeddings", status, 0, 0, time.Since(start), true)
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	metrics.Record(req.Model, upstream, app, "/api/embeddings", 200, 0, 0, time.Since(start), false)
	writeJSON(w, 200, ollamaEmbeddingsResponse{Embedding: emb})
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
	backend, upstream, rerr := cfg.route(clientModel)
	if rerr != nil {
		metrics.Record(clientModel, clientModel, app, "/v1/embeddings", 400, 0, 0, time.Since(start), true)
		writeJSON(w, 400, map[string]string{"error": rerr.Error()})
		return
	}
	if _, ok := body["model"].(string); ok {
		body["model"] = upstream
	}
	b, err := json.Marshal(body)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "internal error"})
		return
	}
	var usage Usage
	status, err := forwardRaw(backend, "/v1/embeddings", b, w, &usage)
	if err != nil && status == 0 {
		status = 502
		writeJSON(w, 502, map[string]string{"error": err.Error()})
	}
	isErr := err != nil || status >= 400
	metrics.Record(clientModel, upstream, app, "/v1/embeddings", status, 0, 0, time.Since(start), isErr)
}
