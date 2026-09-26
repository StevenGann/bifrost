package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 300 * time.Second}

// httpClientLocal uses a short dial timeout (3s): LAN backends respond in
// milliseconds, so a powered-off nomadic worker fails fast instead of hanging
// on the default 30s dial timeout before the circuit breaker can open.
var httpClientLocal = &http.Client{
	Timeout: 300 * time.Second,
	Transport: func() *http.Transport {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DialContext = (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		return t
	}(),
}

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
// content delta. It returns committed=true once the first chunk is about to
// reach the client, after which retry/failover is impossible. Pre-commit
// failures (connect, non-200) return committed=false so the caller can retry
// or fail over transparently. Token usage is written to *usage.
func streamChat(b Backend, oreq OpenAIRequest, onChunk func(content string) error, usage *Usage) (committed bool, err error) {
	oreq.Stream = true
	oreq.StreamOptions = &StreamOptions{IncludeUsage: true}
	body, err := json.Marshal(oreq)
	if err != nil {
		return false, err
	}
	req, err := upstreamReq(b, "POST", "/v1/chat/completions", body)
	if err != nil {
		return false, err
	}
	resp, err := do(b, req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return false, fmt.Errorf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
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
			committed = true // about to write the first/next chunk to the client
			if err := onChunk(content); err != nil {
				return committed, err
			}
		}
	}
	if scanner.Err() != nil {
		return committed, scanner.Err()
	}
	return committed, nil
}

// chat performs a non-streaming chat completion and returns the parsed body
// plus token usage. Errors are *upstreamError so callers can tell transient
// failures from permanent ones.
func chat(b Backend, oreq OpenAIRequest) (*OpenAIChatResponse, Usage, error) {
	oreq.Stream = false
	body, err := json.Marshal(oreq)
	if err != nil {
		return nil, Usage{}, err
	}
	req, err := upstreamReq(b, "POST", "/v1/chat/completions", body)
	if err != nil {
		return nil, Usage{}, err
	}
	resp, err := do(b, req)
	if err != nil {
		return nil, Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, Usage{}, &upstreamError{resp.StatusCode, fmt.Sprintf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))}
	}
	var out OpenAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, Usage{}, &upstreamError{200, err.Error()}
	}
	return &out, out.Usage.toUsage(), nil
}

// rawAttempt performs one /v1 passthrough attempt, returning the upstream
// response on success (2xx and client-error 4xx pass through untouched).
// Transient failures (connection, 429, 5xx) return an error so the caller can
// retry/fail over before anything reaches the client. Streaming requests get
// stream_options.include_usage injected.
func rawAttempt(b Backend, path string, body []byte) (*http.Response, error) {
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
		return nil, err
	}
	resp, err := do(b, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		return nil, &upstreamError{resp.StatusCode, fmt.Sprintf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))}
	}
	return resp, nil
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
