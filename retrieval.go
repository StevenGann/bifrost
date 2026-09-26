package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

type documentInput struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

type documentsRequest struct {
	Documents []documentInput `json:"documents"`
}

type documentsResponse struct {
	Indexed int `json:"indexed"`
	Chunks  int `json:"chunks"`
}

type retrieveRequest struct {
	Query string `json:"query"`
	K     int    `json:"k"`
}

type retrieveResponse struct {
	Chunks []scoredChunk `json:"chunks"`
}

// chunkText splits text into fixed-size rune chunks with overlap, trimming each
// piece and dropping empties.
func chunkText(text string, size, overlap int) []string {
	if size <= 0 {
		size = 1000
	}
	if overlap < 0 || overlap >= size {
		overlap = 0
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	step := size - overlap
	runes := []rune(text)
	var out []string
	for start := 0; start < len(runes); start += step {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, strings.TrimSpace(string(runes[start:end])))
		if end == len(runes) {
			break
		}
	}
	return out
}

// lastUserContent returns the most recent user message's text.
func lastUserContent(messages []OpenAIMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

// ingestDocuments chunks and embeds each document, storing chunks in the global
// index. Returns the number of chunks indexed (and any partial progress on a
// mid-way embedding failure).
func (c Config) ingestDocuments(docs []documentInput) (int, error) {
	total := 0
	for _, d := range docs {
		for _, piece := range chunkText(d.Text, c.ChunkSize, c.ChunkOverlap) {
			emb, err := c.embedQuery(piece)
			if err != nil {
				return total, err
			}
			docIndex.Add(chunk{Text: piece, Embedding: emb, DocName: d.Name})
			total++
		}
	}
	return total, nil
}

// retrieve returns the top-k chunks for a query.
func (c Config) retrieve(query string, k int) ([]scoredChunk, error) {
	emb, err := c.embedQuery(query)
	if err != nil {
		return nil, err
	}
	return docIndex.Search(emb, k), nil
}

// retrieveContext formats the top-k chunks as a numbered context block for RAG,
// or "" when nothing was retrieved.
func (c Config) retrieveContext(query string) (string, error) {
	hits, err := c.retrieve(query, c.RetrieveK)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "", nil
	}
	var sb strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&sb, "[%d] %s\n\n", i+1, h.Text)
	}
	return strings.TrimSpace(sb.String()), nil
}

// contextSystem wraps retrieved context in a system prompt that tells the model
// to ground its answer and to say so when the context is silent.
func contextSystem(system, ctx string) string {
	prefix := "Use the following retrieved context to answer the question. If the context does not contain the answer, say so.\n\n" + ctx
	if system == "" {
		return prefix
	}
	return prefix + "\n\n" + system
}

// prependContext injects a system message carrying retrieved document context.
func prependContext(messages []OpenAIMessage, ctx string) []OpenAIMessage {
	sys := OpenAIMessage{Role: "system", Content: contextSystem("", ctx)}
	return append([]OpenAIMessage{sys}, messages...)
}

func handleDocuments(w http.ResponseWriter, r *http.Request, cfg Config) {
	app := appFrom(r)
	start := time.Now()
	var req documentsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	if len(req.Documents) == 0 {
		writeJSON(w, 400, map[string]string{"error": "no documents"})
		return
	}
	n, err := cfg.ingestDocuments(req.Documents)
	if err != nil {
		status := errorStatus(statusOf(err))
		metrics.Record("documents", cfg.EmbedModel, app, "/api/documents", status, 0, 0, time.Since(start), true)
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	if cfg.IndexFile != "" {
		if err := docIndex.Save(cfg.IndexFile); err != nil {
			log.Printf("index save failed: %v", err)
		}
	}
	metrics.Record("documents", cfg.EmbedModel, app, "/api/documents", 200, 0, 0, time.Since(start), false)
	writeJSON(w, 200, documentsResponse{Indexed: len(req.Documents), Chunks: n})
}

func handleRetrieve(w http.ResponseWriter, r *http.Request, cfg Config) {
	app := appFrom(r)
	start := time.Now()
	var req retrieveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	k := req.K
	if k <= 0 {
		k = cfg.RetrieveK
	}
	hits, err := cfg.retrieve(req.Query, k)
	if err != nil {
		status := errorStatus(statusOf(err))
		metrics.Record("retrieve", cfg.EmbedModel, app, "/api/retrieve", status, 0, 0, time.Since(start), true)
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	metrics.Record("retrieve", cfg.EmbedModel, app, "/api/retrieve", 200, 0, 0, time.Since(start), false)
	writeJSON(w, 200, retrieveResponse{Chunks: hits})
}
