package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// ---- Ollama-native types ----

type ollamaModel struct {
	Name       string `json:"name"`
	Model      string `json:"model"`
	ModifiedAt string `json:"modified_at"`
	Size       int64  `json:"size"`
	Digest     string `json:"digest"`
	Details    struct {
		Format            string `json:"format"`
		Family            string `json:"family"`
		ParameterSize     string `json:"parameter_size"`
		QuantizationLevel string `json:"quantization_level"`
	} `json:"details"`
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []OpenAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Options  struct {
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
		NumPredict  int      `json:"num_predict"`
	} `json:"options"`
	Format string `json:"format"`
}

type ollamaGenerateRequest struct {
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	System  string `json:"system"`
	Stream  bool   `json:"stream"`
	Format  string `json:"format"`
	Options struct {
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
		NumPredict  int      `json:"num_predict"`
	} `json:"options"`
}

type ollamaStreamMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaStreamChunk struct {
	Model     string          `json:"model"`
	CreatedAt string          `json:"created_at"`
	Message   ollamaStreamMsg `json:"message"`
	Done      bool            `json:"done"`
}

type ollamaChatResponse struct {
	Model     string          `json:"model"`
	CreatedAt string          `json:"created_at"`
	Message   ollamaStreamMsg `json:"message"`
	Done      bool            `json:"done"`
}

type ollamaGenerateResponse struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Response  string `json:"response"`
	Done      bool   `json:"done"`
}

// ---- handlers ----

func handleTags(w http.ResponseWriter, cfg Config) {
	names := cfg.modelNames()
	models := make([]ollamaModel, 0, len(names))
	for _, name := range names {
		om := ollamaModel{Name: name, Model: name, ModifiedAt: nowRFC3339(), Size: 0, Digest: "sha256:bifrost-proxy"}
		om.Details.Format = "gguf"
		om.Details.Family = "bifrost"
		models = append(models, om)
	}
	writeJSON(w, 200, map[string]any{"models": models})
}

// toOpenAIRequest translates an Ollama-native chat request into an
// OpenAI-compatible request, applying option defaults. The model name is the
// already-resolved upstream name.
func toOpenAIRequest(upstreamModel string, messages []OpenAIMessage, format string, temperature, topP *float64, numPredict int) OpenAIRequest {
	oreq := OpenAIRequest{
		Model:       upstreamModel,
		Messages:    messages,
		Temperature: 0.7,
		TopP:        0.9,
	}
	if temperature != nil {
		oreq.Temperature = *temperature
	}
	if topP != nil {
		oreq.TopP = *topP
	}
	if numPredict > 0 {
		oreq.MaxTokens = numPredict
	}
	if format == "json" {
		oreq.ResponseFormat = &ResponseFormat{Type: "json_object"}
	}
	return oreq
}

func handleChat(w http.ResponseWriter, r *http.Request, cfg Config) {
	app := appFrom(r)
	start := time.Now()
	var req ollamaChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	clientModel := req.Model

	if req.Stream {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fl, ok := w.(http.Flusher)
		if !ok {
			writeJSON(w, 500, map[string]string{"error": "streaming unsupported"})
			return
		}
		enc := json.NewEncoder(w)
		var usage Usage
		committed := false
		err, lastUpstream, retries, fallbacks := cfg.resolve(req.Model, func(b Backend, up string) (error, bool) {
			oreq := toOpenAIRequest(up, req.Messages, req.Format, req.Options.Temperature, req.Options.TopP, req.Options.NumPredict)
			c, e := streamChat(b, oreq, func(content string) error {
				committed = true
				if err := enc.Encode(ollamaStreamChunk{
					Model:     req.Model,
					CreatedAt: nowRFC3339(),
					Message:   ollamaStreamMsg{Role: "assistant", Content: content},
					Done:      false,
				}); err != nil {
					return err
				}
				fl.Flush()
				return nil
			}, &usage)
			return e, c
		})
		if retries > 0 {
			metrics.RecordRetries(clientModel, app, retries)
		}
		if fallbacks > 0 {
			metrics.RecordFallbacks(clientModel, app, fallbacks)
		}
		if err != nil && !committed {
			status := errorStatus(statusOf(err))
			metrics.Record(clientModel, lastUpstream, app, "/api/chat", status, 0, 0, time.Since(start), true)
			w.Header().Set("Content-Type", "application/json")
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		_ = enc.Encode(ollamaStreamChunk{Model: req.Model, CreatedAt: nowRFC3339(), Message: ollamaStreamMsg{Role: "assistant"}, Done: true})
		fl.Flush()
		if err != nil {
			log.Printf("chat stream error: %v", err)
		}
		metrics.Record(clientModel, lastUpstream, app, "/api/chat", 200, usage.PromptTokens, usage.CompletionTokens, time.Since(start), err != nil)
		return
	}

	// Non-streaming: retry + fallback, fronted by an exact-match response cache.
	resp, _, status, err := cfg.completeCached(cacheKey("/api/chat", req), clientModel, app, "/api/chat", func(up string) OpenAIRequest {
		return toOpenAIRequest(up, req.Messages, req.Format, req.Options.Temperature, req.Options.TopP, req.Options.NumPredict)
	})
	if err != nil {
		writeJSON(w, errorStatus(status), map[string]string{"error": err.Error()})
		return
	}
	msg := ollamaStreamMsg{}
	if len(resp.Choices) > 0 {
		msg = ollamaStreamMsg{Role: resp.Choices[0].Message.Role, Content: resp.Choices[0].Message.Content}
	}
	writeJSON(w, 200, ollamaChatResponse{Model: req.Model, CreatedAt: nowRFC3339(), Message: msg, Done: true})
}

func handleGenerate(w http.ResponseWriter, r *http.Request, cfg Config) {
	app := appFrom(r)
	start := time.Now()
	var req ollamaGenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	clientModel := req.Model

	messages := make([]OpenAIMessage, 0, 2)
	if req.System != "" {
		messages = append(messages, OpenAIMessage{Role: "system", Content: req.System})
	}
	messages = append(messages, OpenAIMessage{Role: "user", Content: req.Prompt})

	if req.Stream {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fl, ok := w.(http.Flusher)
		if !ok {
			writeJSON(w, 500, map[string]string{"error": "streaming unsupported"})
			return
		}
		enc := json.NewEncoder(w)
		var usage Usage
		committed := false
		err, lastUpstream, retries, fallbacks := cfg.resolve(req.Model, func(b Backend, up string) (error, bool) {
			oreq := toOpenAIRequest(up, messages, req.Format, req.Options.Temperature, req.Options.TopP, req.Options.NumPredict)
			c, e := streamChat(b, oreq, func(content string) error {
				committed = true
				if err := enc.Encode(ollamaGenerateResponse{Model: req.Model, CreatedAt: nowRFC3339(), Response: content, Done: false}); err != nil {
					return err
				}
				fl.Flush()
				return nil
			}, &usage)
			return e, c
		})
		if retries > 0 {
			metrics.RecordRetries(clientModel, app, retries)
		}
		if fallbacks > 0 {
			metrics.RecordFallbacks(clientModel, app, fallbacks)
		}
		if err != nil && !committed {
			status := errorStatus(statusOf(err))
			metrics.Record(clientModel, lastUpstream, app, "/api/generate", status, 0, 0, time.Since(start), true)
			w.Header().Set("Content-Type", "application/json")
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		_ = enc.Encode(ollamaGenerateResponse{Model: req.Model, CreatedAt: nowRFC3339(), Response: "", Done: true})
		fl.Flush()
		if err != nil {
			log.Printf("generate stream error: %v", err)
		}
		metrics.Record(clientModel, lastUpstream, app, "/api/generate", 200, usage.PromptTokens, usage.CompletionTokens, time.Since(start), err != nil)
		return
	}

	// Non-streaming: retry + fallback, fronted by an exact-match response cache.
	resp, _, status, err := cfg.completeCached(cacheKey("/api/generate", req), clientModel, app, "/api/generate", func(up string) OpenAIRequest {
		return toOpenAIRequest(up, messages, req.Format, req.Options.Temperature, req.Options.TopP, req.Options.NumPredict)
	})
	if err != nil {
		writeJSON(w, errorStatus(status), map[string]string{"error": err.Error()})
		return
	}
	content := ""
	if len(resp.Choices) > 0 {
		content = resp.Choices[0].Message.Content
	}
	writeJSON(w, 200, ollamaGenerateResponse{Model: req.Model, CreatedAt: nowRFC3339(), Response: content, Done: true})
}
