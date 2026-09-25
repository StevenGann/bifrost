package main

import (
	"fmt"
	"strings"
	"time"
)

// backoffBase is the initial retry delay; it doubles each retry.
const backoffBase = 500 * time.Millisecond

// upstreamError is a failure from an upstream attempt. status is the HTTP
// status (0 for a transport/dial failure; 200 for a decode failure on an
// otherwise-OK response).
type upstreamError struct {
	status int
	msg    string
}

func (e *upstreamError) Error() string { return e.msg }

// transient reports whether a failure is worth retrying: transport errors
// (status 0), rate-limits (429), or server errors (5xx).
func transient(err error) bool {
	ue, ok := err.(*upstreamError)
	if !ok {
		return false
	}
	return ue.status == 0 || ue.status == 429 || ue.status >= 500
}

func statusOf(err error) int {
	if ue, ok := err.(*upstreamError); ok {
		return ue.status
	}
	return 0
}

// errorStatus maps a final attempt status onto an HTTP status for the client.
func errorStatus(status int) int {
	if status >= 400 && status <= 599 {
		return status
	}
	return 502
}

// retryChat runs chat() with retry on transient failures, returning the number
// of retries performed.
func retryChat(b Backend, oreq OpenAIRequest, maxRetries int) (*OpenAIChatResponse, Usage, int, error, int) {
	attempt := 0
	for {
		resp, usage, err := chat(b, oreq)
		if err == nil {
			return resp, usage, 200, nil, attempt
		}
		if !transient(err) || attempt >= maxRetries {
			return nil, Usage{}, statusOf(err), err, attempt
		}
		time.Sleep(backoffBase * time.Duration(1<<uint(attempt)))
		attempt++
	}
}

// complete routes a client model to its backend, retries transient failures,
// and walks the FALLBACKS chain on final failure. It records all metrics
// internally (final request, retries, and fallbacks). build constructs the
// OpenAI request for a resolved upstream model name.
func (c Config) complete(clientModel, app, endpoint string, build func(upstream string) OpenAIRequest) (*OpenAIChatResponse, Usage, int, error) {
	start := time.Now()
	wantsLocal := strings.HasPrefix(clientModel, "private:")
	cur := clientModel
	seen := map[string]bool{}
	retries := 0
	fellBack := false
	var lastErr error
	lastStatus := 502
	lastUpstream := cur

	for {
		if seen[cur] {
			break
		}
		seen[cur] = true
		b, up, rerr := c.route(cur)
		if rerr != nil {
			metrics.Record(clientModel, clientModel, app, endpoint, 400, 0, 0, time.Since(start), true)
			return nil, Usage{}, 400, rerr
		}
		if wantsLocal && !b.Local {
			e := fmt.Errorf("model %q requires a local backend but %q is not local", clientModel, cur)
			metrics.Record(clientModel, clientModel, app, endpoint, 400, 0, 0, time.Since(start), true)
			return nil, Usage{}, 400, e
		}
		r, u, st, err, n := retryChat(b, build(up), c.Retries)
		retries += n
		lastStatus = st
		lastErr = err
		lastUpstream = up
		if err == nil {
			metrics.Record(clientModel, up, app, endpoint, st, u.PromptTokens, u.CompletionTokens, time.Since(start), false)
			if retries > 0 {
				metrics.RecordRetries(clientModel, app, retries)
			}
			if fellBack {
				metrics.RecordFallbacks(clientModel, app, 1)
			}
			return r, u, st, nil
		}
		next := c.Fallbacks[cur]
		if next == "" {
			break
		}
		fellBack = true
		cur = next
	}

	metrics.Record(clientModel, lastUpstream, app, endpoint, lastStatus, 0, 0, time.Since(start), true)
	if retries > 0 {
		metrics.RecordRetries(clientModel, app, retries)
	}
	if fellBack {
		metrics.RecordFallbacks(clientModel, app, 1)
	}
	return nil, Usage{}, lastStatus, lastErr
}
