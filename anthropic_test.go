package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The translation is the whole point of the Anthropic dialect: if a field is
// dropped or renamed wrongly, the upstream either rejects the request or
// silently answers a different question.

func TestAnthropicRequestBody(t *testing.T) {
	in := []byte(`{
		"model":"claude",
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":"hello"},
			{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"result text"},
			{"role":"user","content":[{"type":"text","text":"and now?"}]}
		],
		"temperature":0.3,
		"stop":"END",
		"tools":[{"type":"function","function":{"name":"lookup","description":"find","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],
		"tool_choice":"auto"
	}`)

	out, err := anthropicRequestBody(in, "claude-sonnet-4")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}

	if got["model"] != "claude-sonnet-4" {
		t.Errorf("model = %v, want the routed upstream name", got["model"])
	}
	if got["max_tokens"] == nil {
		t.Errorf("max_tokens missing; Anthropic rejects the request without it")
	}
	if got["system"] != "be brief" {
		t.Errorf("system = %v, want the system message lifted out", got["system"])
	}
	if got["temperature"] != 0.3 {
		t.Errorf("temperature = %v", got["temperature"])
	}
	stop, _ := got["stop_sequences"].([]any)
	if len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stop_sequences = %v, want [END]", got["stop_sequences"])
	}

	msgs, _ := got["messages"].([]any)
	// user / assistant / user — the system turn is lifted out, and the tool
	// result merges into the following user turn because Anthropic rejects
	// two turns with the same role in a row.
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3: %v", len(msgs), msgs)
	}
	roles := make([]string, len(msgs))
	for i, m := range msgs {
		mm := m.(map[string]any)
		roles[i], _ = mm["role"].(string)
		if roles[i] == "system" {
			t.Errorf("system message leaked into messages")
		}
	}

	// The assistant turn must carry a tool_use block, and the tool result a
	// tool_result block inside a user turn — that is how Anthropic represents
	// the same exchange.
	assistant := msgs[1].(map[string]any)
	ablocks, _ := assistant["content"].([]any)
	if len(ablocks) != 1 || ablocks[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("assistant content = %v, want one tool_use block", assistant["content"])
	}
	if ablocks[0].(map[string]any)["input"].(map[string]any)["q"] != "x" {
		t.Errorf("tool_use input was not parsed from the arguments string")
	}

	toolResult := msgs[2].(map[string]any)
	rblocks, _ := toolResult["content"].([]any)
	if toolResult["role"] != "user" || rblocks[0].(map[string]any)["type"] != "tool_result" {
		t.Fatalf("tool message = %v, want a user turn with tool_result", toolResult)
	}
	if rblocks[0].(map[string]any)["tool_use_id"] != "call_1" {
		t.Errorf("tool_use_id not carried across")
	}
	// The following user turn was merged in; its text must survive the merge.
	if len(rblocks) != 2 {
		t.Fatalf("merged turn has %d blocks, want tool_result + text", len(rblocks))
	}
	if rblocks[1].(map[string]any)["text"] != "and now?" {
		t.Errorf("text after the tool result was lost in the merge: %v", rblocks[1])
	}

	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want 1", got["tools"])
	}
	tm := tools[0].(map[string]any)
	if tm["name"] != "lookup" || tm["input_schema"] == nil {
		t.Errorf("tool = %v, want name + input_schema", tm)
	}

	// Nothing OpenAI-specific may leak through: Anthropic 400s on unknown keys.
	for _, banned := range []string{"tools_choice", "stop", "stream_options", "response_format"} {
		if _, ok := got[banned]; ok {
			t.Errorf("OpenAI-only field %q leaked into the Messages request", banned)
		}
	}
}

func TestAnthropicMaxTokensDefaults(t *testing.T) {
	out, err := anthropicRequestBody([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "m")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["max_tokens"] == nil || got["max_tokens"].(float64) <= 0 {
		t.Fatalf("max_tokens = %v, want a positive default", got["max_tokens"])
	}
}

func TestAnthropicResponseTranslation(t *testing.T) {
	raw := []byte(`{
		"id":"msg_1","model":"claude-sonnet-4",
		"content":[{"type":"text","text":"the answer"},{"type":"tool_use","id":"tu_1","name":"lookup","input":{"q":"x"}}],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":12,"output_tokens":7,"cache_read_input_tokens":3}
	}`)

	out, usage, err := openAIFromAnthropic(raw, "claude")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if usage.PromptTokens != 12 || usage.CompletionTokens != 7 || usage.CachedTokens != 3 {
		t.Errorf("usage = %+v, want 12/7/3", usage)
	}

	var got struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d", len(got.Choices))
	}
	c := got.Choices[0]
	if c.Message.Content == nil || *c.Message.Content != "the answer" {
		t.Errorf("content = %v", c.Message.Content)
	}
	if c.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls (from tool_use)", c.FinishReason)
	}
	if len(c.Message.ToolCalls) != 1 || c.Message.ToolCalls[0].Function.Name != "lookup" {
		t.Fatalf("tool_calls = %+v", c.Message.ToolCalls)
	}
	if !strings.Contains(c.Message.ToolCalls[0].Function.Arguments, `"q"`) {
		t.Errorf("tool arguments = %q, want the JSON input re-serialized", c.Message.ToolCalls[0].Function.Arguments)
	}
	if got.Usage.TotalTokens != 19 {
		t.Errorf("total_tokens = %d, want 19", got.Usage.TotalTokens)
	}
}

func TestAnthropicStreamTranslation(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":9}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" there"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	w := httptest.NewRecorder()
	var usage Usage
	if err := copyAnthropicStream(strings.NewReader(sse), w, "claude", &usage); err != nil {
		t.Fatalf("stream: %v", err)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"Hello"`) || !strings.Contains(body, `"content":" there"`) {
		t.Errorf("text deltas missing:\n%s", body)
	}
	if !strings.Contains(body, `data: [DONE]`) {
		t.Errorf("stream did not terminate with [DONE]")
	}
	if usage.PromptTokens != 9 || usage.CompletionTokens != 4 {
		t.Errorf("usage = %+v, want 9 prompt / 4 completion", usage)
	}

	// Every emitted line must be valid JSON inside the data: prefix.
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
			t.Errorf("chunk is not JSON: %q", line)
		}
		if v["object"] != "chat.completion.chunk" {
			t.Errorf("chunk object = %v", v["object"])
		}
	}
}

func TestAnthropicAttemptUsesVendorAuth(t *testing.T) {
	var path, key, ver, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key, ver, auth = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	b := Backend{Name: "anthropic", BaseURL: srv.URL, APIKey: "sk-ant-test", Kind: "anthropic"}
	resp, err := anthropicAttempt(b, []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	defer resp.Body.Close()

	if path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", path)
	}
	if key != "sk-ant-test" {
		t.Errorf("x-api-key not sent")
	}
	if ver == "" {
		t.Errorf("anthropic-version not sent; the API rejects requests without it")
	}
	if auth != "" {
		t.Errorf("Authorization header leaked: Anthropic authenticates with x-api-key, not a bearer token")
	}
}

func TestAnthropicErrorsBecomeOpenAIErrors(t *testing.T) {
	raw := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: required"}}`)
	out, _, err := openAIFromAnthropic(raw, "claude")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if got.Error.Message == "" {
		t.Fatalf("error message lost: %s", string(out))
	}
}

// Some Claude models reject sampling parameters. The gateway retries once
// without them rather than failing a request the caller could not have known
// was unsupported.
func TestAnthropicRetriesWithoutSamplingParams(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"temperature"`) {
			w.WriteHeader(400)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"temperature is not supported for this model"}}`)
			return
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"id":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	b := Backend{Name: "a", BaseURL: srv.URL, APIKey: "k", Kind: "anthropic"}
	resp, err := anthropicAttempt(b, []byte(`{"model":"m","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	defer resp.Body.Close()

	if hits != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (original + retry)", hits)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 after the retry", resp.StatusCode)
	}
}

// A non-sampling 400 must not be retried; it is a real client error.
func TestAnthropicDoesNotRetryOther400s(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(400)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"credit balance is too low"}}`)
	}))
	defer srv.Close()

	b := Backend{Name: "a", BaseURL: srv.URL, APIKey: "k", Kind: "anthropic"}
	resp, err := anthropicAttempt(b, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	defer resp.Body.Close()
	if hits != 1 {
		t.Fatalf("upstream saw %d requests, want 1", hits)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400 passed through", resp.StatusCode)
	}
}
