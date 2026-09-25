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

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// OpenAIRequest is the OpenAI chat-completion request we build when translating
// Ollama-native calls. (The /v1 pass-through path instead forwards a generic
// JSON object so unknown fields survive.)
type OpenAIRequest struct {
	Model          string          `json:"model"`
	Messages       []OpenAIMessage `json:"messages"`
	Stream         bool            `json:"stream"`
	Temperature    float64         `json:"temperature"`
	TopP           float64         `json:"top_p"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	StreamOptions  *StreamOptions  `json:"stream_options,omitempty"`
}

// OpenAIUsage mirrors the usage block DeepSeek/OpenAI return.
type OpenAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// Usage is the flattened token accounting surfaced to metrics.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
}

func (u *OpenAIUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		CachedTokens:     u.PromptTokensDetails.CachedTokens,
	}
}

type OpenAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *OpenAIUsage `json:"usage"`
}

type OpenAIChatResponse struct {
	Choices []struct {
		Message OpenAIMessage `json:"message"`
	} `json:"choices"`
	Usage OpenAIUsage `json:"usage"`
}

func upstreamReq(b Backend, method, path string, body []byte) (*http.Request, error) {
	body = maybeRedact(b, body)
	req, err := http.NewRequest(method, b.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
	return req, nil
}

// streamChat POSTs a streaming chat completion and calls onChunk for each
// content delta. It injects stream_options.include_usage so the final chunk
// carries token usage, which is written to *usage.
func streamChat(b Backend, oreq OpenAIRequest, onChunk func(content string) error, usage *Usage) error {
	oreq.Stream = true
	oreq.StreamOptions = &StreamOptions{IncludeUsage: true}
	body, err := json.Marshal(oreq)
	if err != nil {
		return err
	}
	req, err := upstreamReq(b, "POST", "/chat/completions", body)
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
		if chunk.Usage != nil {
			*usage = chunk.Usage.toUsage()
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

// chat performs a non-streaming chat completion and returns the parsed body
// plus token usage.
func chat(b Backend, oreq OpenAIRequest) (*OpenAIChatResponse, Usage, error) {
	oreq.Stream = false
	body, err := json.Marshal(oreq)
	if err != nil {
		return nil, Usage{}, err
	}
	req, err := upstreamReq(b, "POST", "/chat/completions", body)
	if err != nil {
		return nil, Usage{}, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, Usage{}, fmt.Errorf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out OpenAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, Usage{}, err
	}
	return &out, out.Usage.toUsage(), nil
}

// forwardRaw forwards an upstream request and copies its response body verbatim
// (used by the /v1 pass-through), while capturing token usage. Streaming
// requests get stream_options.include_usage injected so the final chunk carries
// usage. It returns the HTTP status written (0 if nothing was written because
// the dial/request failed before any headers went out).
func forwardRaw(b Backend, path string, body []byte, w http.ResponseWriter, usage *Usage) (int, error) {
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	if probe.Stream {
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			m["stream_options"] = map[string]any{"include_usage": true}
			if b, err := json.Marshal(m); err == nil {
				body = b
			}
		}
	}

	req, err := upstreamReq(b, "POST", path, body)
	if err != nil {
		return 0, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		_, err = io.Copy(w, resp.Body)
		return resp.StatusCode, err
	}
	if probe.Stream {
		err = copyStream(resp.Body, w, usage)
	} else {
		err = copyBody(resp.Body, w, usage)
	}
	return resp.StatusCode, err
}

// copyStream copies an SSE stream verbatim (preserving line endings) while
// capturing the final chunk's usage.
func copyStream(r io.Reader, w http.ResponseWriter, usage *Usage) error {
	br := bufio.NewReader(r)
	fl, _ := w.(http.Flusher)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
			parseSSEUsage(line, usage)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// copyBody reads a non-streaming JSON body, capturing usage, then writes it
// verbatim.
func copyBody(r io.Reader, w http.ResponseWriter, usage *Usage) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	var out OpenAIChatResponse
	if json.Unmarshal(b, &out) == nil {
		*usage = out.Usage.toUsage()
	}
	_, err = w.Write(b)
	return err
}

func parseSSEUsage(line []byte, usage *Usage) {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return
	}
	data := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if data == "[DONE]" {
		return
	}
	var chunk OpenAIStreamChunk
	if json.Unmarshal([]byte(data), &chunk) == nil && chunk.Usage != nil {
		*usage = chunk.Usage.toUsage()
	}
}
