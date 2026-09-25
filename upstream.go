package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 300 * time.Second}

// OpenAIMessage is a single chat message in OpenAI format (also used for
// Ollama's messages field, which is structurally identical).
type OpenAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ResponseFormat struct {
	Type string `json:"type"`
}

// OpenAIRequest is the OpenAI chat-completion request we build when
// translating Ollama-native calls. (The /v1 pass-through path instead
// forwards a generic JSON object so unknown fields survive.)
type OpenAIRequest struct {
	Model          string          `json:"model"`
	Messages       []OpenAIMessage `json:"messages"`
	Stream         bool            `json:"stream"`
	Temperature    float64         `json:"temperature"`
	TopP           float64         `json:"top_p"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

type OpenAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

type OpenAIChatResponse struct {
	Choices []struct {
		Message OpenAIMessage `json:"message"`
	} `json:"choices"`
}

func upstreamReq(cfg Config, method, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequest(method, cfg.UpstreamBase+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.UpstreamKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.UpstreamKey)
	}
	return req, nil
}

// streamChat POSTs a streaming chat completion and calls onChunk for each
// content delta. The callback returns an error to abort early.
func streamChat(cfg Config, oreq OpenAIRequest, onChunk func(content string) error) error {
	oreq.Stream = true
	body, err := json.Marshal(oreq)
	if err != nil {
		return err
	}
	req, err := upstreamReq(cfg, "POST", "/chat/completions", body)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk OpenAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			content := chunk.Choices[0].Delta.Content
			if content == "" {
				continue
			}
			if err := onChunk(content); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

// chat performs a non-streaming chat completion and returns the parsed body.
func chat(cfg Config, oreq OpenAIRequest) (*OpenAIChatResponse, error) {
	oreq.Stream = false
	body, err := json.Marshal(oreq)
	if err != nil {
		return nil, err
	}
	req, err := upstreamReq(cfg, "POST", "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out OpenAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// forwardRaw forwards an upstream request and copies its response body
// verbatim (used by the /v1 pass-through, preserving the exact SSE/JSON shape).
func forwardRaw(cfg Config, path string, body []byte, w http.ResponseWriter) error {
	req, err := upstreamReq(cfg, "POST", path, body)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	return err
}
