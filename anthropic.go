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

// Anthropic does not speak the dialect the rest of the gateway speaks. It wants
// /v1/messages rather than /v1/chat/completions, `x-api-key` rather than a
// bearer token, `max_tokens` as a required field, content as blocks rather than
// a string, and its SSE events are named rather than anonymous deltas.
//
// Clients keep talking OpenAI to Bifrost; this file is the seam where a request
// is translated on the way out and the reply is translated on the way back.
// Everything that reports usage (metrics, ledger, budget) sees the OpenAI shape.

const (
	anthropicVersion    = "2023-06-01"
	anthropicDefaultMax = 4096
)

func isAnthropic(b Backend) bool {
	return strings.EqualFold(strings.TrimSpace(b.Kind), "anthropic")
}

// anthropicMessages is the wire request for POST /v1/messages.
type anthropicMessages struct {
	Model         string           `json:"model"`
	MaxTokens     int              `json:"max_tokens"`
	System        any              `json:"system,omitempty"`
	Messages      []map[string]any `json:"messages"`
	Temperature   *float64         `json:"temperature,omitempty"`
	TopP          *float64         `json:"top_p,omitempty"`
	StopSequences []string         `json:"stop_sequences,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
	Tools         []map[string]any `json:"tools,omitempty"`
	ToolChoice    any              `json:"tool_choice,omitempty"`
}

// anthropicRequestBody translates an OpenAI chat request into a Messages
// request. Fields it does not understand are dropped rather than forwarded:
// Anthropic rejects unknown top-level keys outright.
func anthropicRequestBody(raw []byte, upstream string) ([]byte, error) {
	var in struct {
		Messages          []map[string]any `json:"messages"`
		MaxTokens         int              `json:"max_tokens"`
		MaxCompletionToks int              `json:"max_completion_tokens"`
		Temperature       *float64         `json:"temperature"`
		TopP              *float64         `json:"top_p"`
		Stop              any              `json:"stop"`
		Stream            bool             `json:"stream"`
		Tools             []map[string]any `json:"tools"`
		ToolChoice        any              `json:"tool_choice"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}

	out := anthropicMessages{
		Model:       upstream,
		MaxTokens:   in.MaxTokens,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stream:      in.Stream,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = in.MaxCompletionToks
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = anthropicDefaultMax
	}
	out.StopSequences = stopSequences(in.Stop)

	var system []string
	for _, m := range in.Messages {
		role, _ := m["role"].(string)
		content := m["content"]

		switch role {
		case "system", "developer":
			system = append(system, flattenText(content))
			continue
		case "tool":
			// OpenAI puts each tool result in its own message; Anthropic wants
			// tool_result blocks inside a user turn.
			id, _ := m["tool_call_id"].(string)
			out.Messages = append(out.Messages, map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type":        "tool_result",
					"tool_use_id": id,
					"content":     flattenText(content),
				}},
			})
			continue
		case "assistant":
			blocks := contentBlocks(content)
			if tcs, ok := m["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcm, _ := tc.(map[string]any)
					fn, _ := tcm["function"].(map[string]any)
					name, _ := fn["name"].(string)
					id, _ := tcm["id"].(string)
					args := fn["arguments"]
					var input any = map[string]any{}
					switch v := args.(type) {
					case string:
						if strings.TrimSpace(v) != "" {
							_ = json.Unmarshal([]byte(v), &input)
						}
					case nil:
					default:
						input = v
					}
					blocks = append(blocks, map[string]any{
						"type":  "tool_use",
						"id":    id,
						"name":  name,
						"input": input,
					})
				}
			}
			if len(blocks) > 0 {
				out.Messages = append(out.Messages, map[string]any{"role": "assistant", "content": blocks})
			}
			continue
		default: // user
			blocks := contentBlocks(content)
			if len(blocks) == 0 {
				blocks = []any{map[string]any{"type": "text", "text": " "}}
			}
			out.Messages = append(out.Messages, map[string]any{
				"role":    "user",
				"content": blocks,
			})
		}
	}
	if len(system) > 0 {
		out.System = strings.Join(system, "\n\n")
	}

	// Anthropic rejects two turns with the same role in a row, which happens
	// naturally once tool results are lifted into their own message.
	out.Messages = mergeAdjacentRoles(out.Messages)

	for _, t := range in.Tools {
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		desc, _ := fn["description"].(string)
		params := fn["parameters"]
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out.Tools = append(out.Tools, map[string]any{
			"name":         name,
			"description":  desc,
			"input_schema": params,
		})
	}
	if len(out.Tools) > 0 {
		out.ToolChoice = anthropicToolChoice(in.ToolChoice)
	}

	return json.Marshal(out)
}

func anthropicToolChoice(v any) any {
	switch c := v.(type) {
	case string:
		switch c {
		case "none":
			return map[string]any{"type": "none"}
		case "required":
			return map[string]any{"type": "any"}
		default:
			return map[string]any{"type": "auto"}
		}
	case map[string]any:
		if fn, ok := c["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
	}
	return nil
}

func stopSequences(v any) []string {
	switch s := v.(type) {
	case string:
		if s != "" {
			return []string{s}
		}
	case []any:
		var out []string
		for _, x := range s {
			if str, ok := x.(string); ok && str != "" {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// flattenText renders any OpenAI content value as a plain string.
func flattenText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			if s, ok := pm["text"].(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// contentBlocks renders an OpenAI content value as Anthropic blocks.
func contentBlocks(v any) []any {
	if s, ok := v.(string); ok {
		if s == "" {
			// An assistant turn that exists only to carry tool_calls has no
			// text; an empty text block is a 400 from Anthropic.
			return []any{}
		}
		return []any{map[string]any{"type": "text", "text": s}}
	}
	arr, ok := v.([]any)
	if !ok {
		return []any{}
	}
	var blocks []any
	for _, p := range arr {
		pm, _ := p.(map[string]any)
		if pm == nil {
			continue
		}
		typ, _ := pm["type"].(string)
		switch typ {
		case "", "text", "input_text":
			if s, ok := pm["text"].(string); ok && s != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": s})
			}
		case "image_url", "input_image":
			url := ""
			switch u := pm["image_url"].(type) {
			case string:
				url = u
			case map[string]any:
				url, _ = u["url"].(string)
			}
			if url == "" {
				continue
			}
			if strings.HasPrefix(url, "data:") {
				// data:<mime>;base64,<payload>
				rest := strings.TrimPrefix(url, "data:")
				comma := strings.Index(rest, ",")
				if comma < 0 {
					continue
				}
				meta, payload := rest[:comma], rest[comma+1:]
				mime := strings.TrimSuffix(strings.SplitN(meta, ";", 2)[0], ";")
				blocks = append(blocks, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": mime,
						"data":       payload,
					},
				})
				continue
			}
			blocks = append(blocks, map[string]any{
				"type":   "image",
				"source": map[string]any{"type": "url", "url": url},
			})
		}
	}
	return blocks
}

func mergeAdjacentRoles(msgs []map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range msgs {
		if n := len(out); n > 0 && out[n-1]["role"] == m["role"] {
			prev, _ := out[n-1]["content"].([]any)
			cur, _ := m["content"].([]any)
			out[n-1]["content"] = append(prev, cur...)
			continue
		}
		out = append(out, m)
	}
	return out
}

// anthropicAttempt performs one upstream call against a Messages backend.
// Failures that another attempt could fix (429, 5xx) come back as an
// *upstreamError so resolve() retries or fails over, exactly as it does for
// OpenAI-shaped backends.
func anthropicAttempt(b Backend, raw []byte) (*http.Response, error) {
	body, err := anthropicRequestBody(raw, lastUpstreamModel(raw))
	if err != nil {
		return nil, err
	}
	resp, err := anthropicPost(b, body)
	if err != nil {
		return nil, err
	}
	// Some Claude models reject sampling parameters outright. Rather than keep a
	// table of which do, treat the rejection as a signal and retry once without
	// them: a client that sends temperature still gets an answer.
	if resp.StatusCode == 400 && rejectsSampling(resp) {
		stripped, serr := withoutSampling(body)
		if serr == nil {
			return anthropicPost(b, stripped)
		}
	}
	return resp, nil
}

// rejectsSampling peeks at a 400 body without consuming it.
func rejectsSampling(resp *http.Response) bool {
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	resp.Body.Close()
	if err != nil {
		return false
	}
	s := strings.ToLower(string(buf))
	hit := strings.Contains(s, "temperature") || strings.Contains(s, "top_p") || strings.Contains(s, "top_k")
	// Put the body back so the caller can still translate the error.
	resp.Body = io.NopCloser(bytes.NewReader(buf))
	return hit
}

func withoutSampling(body []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	delete(m, "temperature")
	delete(m, "top_p")
	delete(m, "top_k")
	return json.Marshal(m)
}

func anthropicPost(b Backend, body []byte) (*http.Response, error) {
	req, err := http.NewRequest("POST", b.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("x-api-key", b.APIKey)
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

// lastUpstreamModel reads the model the caller already resolved. handleOpenAIChat
// rewrites body["model"] to the upstream name before each attempt, so this is
// the routed model rather than the client's alias.
func lastUpstreamModel(raw []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(raw, &probe)
	return probe.Model
}

// --- response translation -------------------------------------------------

type anthropicResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens          int `json:"input_tokens"`
		OutputTokens         int `json:"output_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func anthropicFinishReason(r string) string {
	switch r {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "stop_sequence", "end_turn":
		return "stop"
	case "":
		return ""
	}
	return r
}

// openAIFromAnthropic turns a Messages reply into a chat completion.
func openAIFromAnthropic(raw []byte, clientModel string) ([]byte, Usage, error) {
	var ar anthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return raw, Usage{}, err
	}
	if ar.Error.Message != "" {
		// Already an error payload; hand it back in the OpenAI error shape.
		out, _ := json.Marshal(map[string]any{
			"error": map[string]any{"message": ar.Error.Message, "type": ar.Error.Type},
		})
		return out, Usage{}, nil
	}

	var text strings.Builder
	var toolCalls []any
	for _, c := range ar.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "tool_use":
			toolCalls = append(toolCalls, map[string]any{
				"id":   c.ID,
				"type": "function",
				"function": map[string]any{
					"name":      c.Name,
					"arguments": string(c.Input),
				},
			})
		}
	}

	msg := map[string]any{"role": "assistant", "content": text.String()}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		if text.Len() == 0 {
			msg["content"] = nil
		}
	}

	usage := Usage{
		PromptTokens:     ar.Usage.InputTokens,
		CompletionTokens: ar.Usage.OutputTokens,
		CachedTokens:     ar.Usage.CacheReadInputTokens,
	}

	out, err := json.Marshal(map[string]any{
		"id":      ar.ID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   clientModel,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": anthropicFinishReason(ar.StopReason),
		}},
		"usage": map[string]any{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.PromptTokens + usage.CompletionTokens,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": usage.CachedTokens,
			},
		},
	})
	return out, usage, err
}

// writeAnthropicResponse buffers a Messages reply and writes it as a chat
// completion. Buffering is required: Anthropic can report an error inside a 200
// body, and the client asked for a non-streaming reply.
func writeAnthropicResponse(resp *http.Response, w http.ResponseWriter, clientModel string, usage *Usage) {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "upstream read failed"})
		return
	}
	out, u, err := openAIFromAnthropic(raw, clientModel)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "upstream decode failed"})
		return
	}
	*usage = u
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

// --- streaming ------------------------------------------------------------

// copyAnthropicStream translates Anthropic's named SSE events into the
// anonymous delta chunks an OpenAI client expects.
func copyAnthropicStream(r io.Reader, w http.ResponseWriter, clientModel string, usage *Usage) error {
	br := bufio.NewReader(r)
	fl, _ := w.(http.Flusher)

	var event string
	var sentRole bool
	// toolIndex tracks which OpenAI tool_calls index each Anthropic content
	// block maps to, so deltas land on the entry they belong to.
	toolIndex := map[int]int{}
	nextTool := 0
	finish := ""
	started := time.Now().Unix()

	emit := func(delta map[string]any, finishReason any) error {
		chunk := map[string]any{
			"id":      "chatcmpl-bifrost",
			"object":  "chat.completion.chunk",
			"created": started,
			"model":   clientModel,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			}},
		}
		b, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			s := strings.TrimRight(string(line), "\r\n")
			switch {
			case strings.HasPrefix(s, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(s, "event:"))
			case strings.HasPrefix(s, "data:"):
				data := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
				if data != "" {
					var ev struct {
						Type    string `json:"type"`
						Index   int    `json:"index"`
						Message struct {
							Usage struct {
								InputTokens int `json:"input_tokens"`
							} `json:"usage"`
						} `json:"message"`
						ContentBlock struct {
							Type string `json:"type"`
							ID   string `json:"id"`
							Name string `json:"name"`
						} `json:"content_block"`
						Delta struct {
							Type        string `json:"type"`
							Text        string `json:"text"`
							PartialJSON string `json:"partial_json"`
							StopReason  string `json:"stop_reason"`
						} `json:"delta"`
						Usage struct {
							OutputTokens int `json:"output_tokens"`
						} `json:"usage"`
					}
					if json.Unmarshal([]byte(data), &ev) == nil {
						typ := ev.Type
						if typ == "" {
							typ = event
						}
						switch typ {
						case "message_start":
							usage.PromptTokens = ev.Message.Usage.InputTokens
							if !sentRole {
								sentRole = true
								if err := emit(map[string]any{"role": "assistant", "content": ""}, nil); err != nil {
									return err
								}
							}
						case "content_block_start":
							if ev.ContentBlock.Type == "tool_use" {
								idx := nextTool
								toolIndex[ev.Index] = idx
								nextTool++
								_ = emit(map[string]any{
									"tool_calls": []any{map[string]any{
										"index": idx,
										"id":    ev.ContentBlock.ID,
										"type":  "function",
										"function": map[string]any{
											"name":      ev.ContentBlock.Name,
											"arguments": "",
										},
									}},
								}, nil)
							}
						case "content_block_delta":
							switch ev.Delta.Type {
							case "text_delta":
								if ev.Delta.Text != "" {
									_ = emit(map[string]any{"content": ev.Delta.Text}, nil)
								}
							case "input_json_delta":
								idx, ok := toolIndex[ev.Index]
								if !ok {
									idx = 0
								}
								if ev.Delta.PartialJSON != "" {
									_ = emit(map[string]any{
										"tool_calls": []any{map[string]any{
											"index": idx,
											"function": map[string]any{
												"arguments": ev.Delta.PartialJSON,
											},
										}},
									}, nil)
								}
							}
						case "message_delta":
							if ev.Delta.StopReason != "" {
								finish = anthropicFinishReason(ev.Delta.StopReason)
							}
							if ev.Usage.OutputTokens > 0 {
								usage.CompletionTokens = ev.Usage.OutputTokens
							}
						case "message_stop":
							var fr any
							if finish != "" {
								fr = finish
							}
							_ = emit(map[string]any{}, fr)
							if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
								return err
							}
							if fl != nil {
								fl.Flush()
							}
							return nil
						case "error":
							return fmt.Errorf("anthropic stream error")
						}
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				// Upstream closed without message_stop; end the stream cleanly so
				// the client does not hang waiting for [DONE].
				_ = emit(map[string]any{}, nil)
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				if fl != nil {
					fl.Flush()
				}
				return nil
			}
			return err
		}
	}
}
