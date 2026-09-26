package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// backoffBase is the initial retry delay; it doubles each retry.
const backoffBase = 500 * time.Millisecond

// errCircuitOpen marks a request refused by a backend's open circuit breaker.
// It is deliberately non-transient: there is no point retrying a known-down
// backend, but the request should still fail over to a fallback.
var errCircuitOpen = errors.New("backend circuit open")

// routeError is a client-side routing failure (unknown model, or a private:*
// model resolving to a non-local backend). Maps to HTTP 400.
type routeError struct{ msg string }

func (e *routeError) Error() string { return e.msg }

// upstreamError is a failure from an upstream attempt. status is the HTTP
// status (0 for a transport/dial failure; 200 for a decode failure on an
// otherwise-OK response).
type upstreamError struct {
	status int
	msg    string
}

func (e *upstreamError) Error() string { return e.msg }

// transient reports whether a failure is worth retrying: transport errors
// (status 0), rate-limits (429), or server errors (5xx). Circuit-open is
// excluded — retrying a known-down backend is pointless (fail over instead).
func transient(err error) bool {
	if errors.Is(err, errCircuitOpen) {
		return false
	}
	ue, ok := err.(*upstreamError)
	if !ok {
		return false
	}
	return ue.status == 0 || ue.status == 429 || ue.status >= 500
}

// statusOf maps an error to a raw HTTP status: 400 for routing errors, 503 for
// a tripped circuit, the upstream status for *upstreamError, 0 otherwise.
func statusOf(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, errCircuitOpen) {
		return 503
	}
	if _, ok := err.(*routeError); ok {
		return 400
	}
	if ue, ok := err.(*upstreamError); ok {
		return ue.status
	}
	return 0
}

// errorStatus maps a raw status onto a client-facing HTTP status.
func errorStatus(status int) int {
	if status >= 400 && status <= 599 {
		return status
	}
	return 502
}

// attemptFn runs one upstream attempt for a resolved (backend, upstream) pair.
// err is nil on success; committed reports that bytes have already started
// flowing to the client (streaming), so retry and failover are impossible.
type attemptFn func(b Backend, upstream string) (err error, committed bool)

// resolve walks the FALLBACKS chain, running attemptFn with retry on transient
// failures. It returns the final error (nil on success), the last upstream
// model attempted, and retry/fallback counts for metrics.
func (c Config) resolve(clientModel string, attempt attemptFn) (err error, lastUpstream string, retries, fallbacks int) {
	wantsLocal := strings.HasPrefix(clientModel, "private:")
	cur := clientModel
	seen := map[string]bool{}
	lastUpstream = clientModel
	var lastErr error

	for {
		if seen[cur] {
			break
		}
		seen[cur] = true

		b, up, rerr := c.route(cur)
		if rerr != nil {
			return &routeError{rerr.Error()}, lastUpstream, retries, fallbacks
		}
		if wantsLocal && !b.Local {
			return &routeError{fmt.Sprintf("model %q requires a local backend but %q is not local", clientModel, cur)}, lastUpstream, retries, fallbacks
		}
		lastUpstream = up

		// Select the first key for this backend (no-op for single-key backends).
		keyID := ""
		if len(b.Keys) > 0 {
			if k, ok := keyRings.pick(b.Name, b.Keys); ok {
				b.APIKey = k.Key
				keyID = k.ID
			}
		}

		err, committed := attempt(b, up)
		hop := 0
		for err != nil && !committed && transient(err) && hop < c.Retries {
			time.Sleep(backoffBase * time.Duration(1<<uint(hop)))
			hop++
			retries++
			// A rate-limited/auth-rejected key is cooled and the retry rotates to
			// the next key in the pool before backend failover ever applies.
			if len(b.Keys) > 0 {
				if keyID != "" && keyRateLimited(err) {
					keyRings.markCooling(b.Name, keyID)
				}
				if k, ok := keyRings.pick(b.Name, b.Keys); ok {
					b.APIKey = k.Key
					keyID = k.ID
					metrics.RecordKeyRotation(b.Name)
				}
			}
			err, committed = attempt(b, up)
		}
		lastErr = err

		if err == nil || committed {
			return err, lastUpstream, retries, fallbacks
		}

		next := c.Fallbacks[cur]
		if next == "" {
			return err, lastUpstream, retries, fallbacks
		}
		fallbacks++
		cur = next
	}
	return lastErr, lastUpstream, retries, fallbacks
}

// complete routes a client model to its backend, retries transient failures,
// and walks the FALLBACKS chain on final failure. build constructs the OpenAI
// request for a resolved upstream model name.
func (c Config) complete(clientModel, app, endpoint string, build func(upstream string) OpenAIRequest) (*OpenAIChatResponse, Usage, int, error) {
	start := time.Now()
	var result *OpenAIChatResponse
	var usage Usage

	err, lastUpstream, retries, fallbacks := c.resolve(clientModel, func(b Backend, up string) (error, bool) {
		r, u, e := chat(b, build(up))
		if e == nil {
			result = r
			usage = u
		}
		return e, false
	})

	if retries > 0 {
		metrics.RecordRetries(clientModel, app, retries)
	}
	if fallbacks > 0 {
		metrics.RecordFallbacks(clientModel, app, fallbacks)
	}

	status := 200
	if err != nil {
		status = statusOf(err)
	}
	metrics.Record(clientModel, lastUpstream, app, endpoint, status, usage.PromptTokens, usage.CompletionTokens, time.Since(start), err != nil)
	return result, usage, status, err
}
