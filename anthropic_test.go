package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	// The system prompt is lifted out as blocks (one text block), because a
	// cache breakpoint has to sit on the last block of the prefix.
	sys, _ := got["system"].([]any)
	if len(sys) != 1 {
		t.Fatalf("system = %v, want one text block", got["system"])
	}
	sysBlock, _ := sys[0].(map[string]any)
	if sysBlock["type"] != "text" || sysBlock["text"] != "be brief" {
		t.Errorf("system block = %v, want {type:text text:be brief}", sysBlock)
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
	// input_tokens (12) is the UNCACHED prompt; the 3 cached tokens are a
	// second, additive count. Reporting 12 would understate the prompt by the
	// portion the cache is serving.
	if usage.PromptTokens != 15 || usage.CompletionTokens != 7 || usage.CachedTokens != 3 {
		t.Errorf("usage = %+v, want 15/7/3 (prompt includes cached)", usage)
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
	if got.Usage.TotalTokens != 22 {
		t.Errorf("total_tokens = %d, want 22 (15 prompt + 7 completion)", got.Usage.TotalTokens)
	}
	if !strings.Contains(string(out), `"cache_write_tokens"`) {
		t.Errorf("usage payload hides the cache-write count:\n%s", out)
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

	// The client must see usage too, not only the ledger: a final chunk with
	// empty choices and a usage object, after the finish chunk and before
	// [DONE] — the OpenAI include_usage shape.
	usageAt := strings.Index(body, `"usage":`)
	doneAt := strings.Index(body, `data: [DONE]`)
	finishAt := strings.Index(body, `"finish_reason":"stop"`)
	if usageAt < 0 {
		t.Fatalf("no usage chunk in stream:\n%s", body)
	}
	if !(finishAt < usageAt && usageAt < doneAt) {
		t.Errorf("usage chunk out of order: finish=%d usage=%d done=%d", finishAt, usageAt, doneAt)
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, `"usage":`) {
			continue
		}
		var c struct {
			Choices []any `json:"choices"`
			Usage   struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(line), "data: ")), &c); err != nil {
			t.Fatalf("usage chunk not JSON: %v", err)
		}
		if len(c.Choices) != 0 {
			t.Errorf("usage chunk choices = %v, want empty", c.Choices)
		}
		if c.Usage.PromptTokens != 9 || c.Usage.CompletionTokens != 4 || c.Usage.TotalTokens != 13 {
			t.Errorf("client usage = %+v, want 9/4/13", c.Usage)
		}
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

func TestAnthropicPromptCacheMarkers(t *testing.T) {
	in := []byte(`{
		"model":"claude",
		"messages":[{"role":"system","content":"long instruction block"},
			{"role":"user","content":"first"}],
		"tools":[{"type":"function","function":{"name":"a","description":"d","parameters":{"type":"object"}}},
		         {"type":"function","function":{"name":"b","description":"d","parameters":{"type":"object"}}}]
	}`)
	t.Setenv("ANTHROPIC_PROMPT_CACHE", "")
	out, err := anthropicRequestBody(in, "claude-opus-5-5")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got struct {
		System []struct {
			CacheControl *struct{ Type string } `json:"cache_control"`
		} `json:"system"`
		Tools []struct {
			CacheControl *struct{ Type string } `json:"cache_control"`
		} `json:"tools"`
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if len(got.System) != 1 || got.System[0].CacheControl == nil {
		t.Errorf("system block has no cache breakpoint: %+v", got.System)
	}
	if len(got.Tools) != 2 || got.Tools[1].CacheControl == nil {
		t.Errorf("last tool has no cache breakpoint: %+v", got.Tools)
	}
	if got.Tools[0].CacheControl != nil {
		t.Errorf("first tool should not carry a breakpoint (only the prefix end does)")
	}
	last := got.Messages[len(got.Messages)-1].Content
	if len(last) == 0 || last[len(last)-1]["cache_control"] == nil {
		t.Errorf("last message block has no cache breakpoint: %+v", last)
	}
	if n := strings.Count(string(out), "cache_control"); n > 3 {
		t.Errorf("%d breakpoints, Anthropic allows 4", n)
	}
}

func TestAnthropicPromptCacheDisable(t *testing.T) {
	t.Setenv("ANTHROPIC_PROMPT_CACHE", "0")
	in := []byte(`{"model":"claude","messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}`)
	out, err := anthropicRequestBody(in, "claude")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if strings.Contains(string(out), "cache_control") {
		t.Errorf("ANTHROPIC_PROMPT_CACHE=0 still emitted breakpoints:\n%s", out)
	}
}

func TestCacheTokenCost(t *testing.T) {
	// Opus list price: $4/M in, $20/M out.
	pc := map[string]pricing{"claude-opus-5-5": {InputPer1M: 4, OutputPer1M: 20}}
	m := newMetrics(pc)
	offPeak := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	// 1M prompt: 900k read (0.1x), 50k written (1.25x), 50k plain.
	u := Usage{PromptTokens: 1_000_000, CachedTokens: 900_000, CacheWriteTokens: 50_000}
	got := m.costAt("claude-opus-5-5", u, offPeak)
	want := 4 * (0.05 + 0.05*1.25 + 0.9*0.1)
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cache-aware cost = %v, want %v (uncached would be 4.0)", got, want)
	}
	// A provider that reports disjoint counts cannot make the sum exceed the prompt.
	u2 := Usage{PromptTokens: 100, CachedTokens: 90, CacheWriteTokens: 90}
	if c := m.costAt("claude-opus-5-5", u2, offPeak); c < 0 {
		t.Errorf("negative cost %v", c)
	}
}
