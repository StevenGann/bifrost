package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SpeechRequest is the OpenAI-compatible /v1/audio/speech body. Bifrost accepts
// the OpenAI shape and translates it per backend kind, so a caller never needs
// to know whether the voice behind a route is ElevenLabs, OpenAI, or a local
// OpenAI-compatible server.
type SpeechRequest struct {
	Model          string   `json:"model"`
	Input          string   `json:"input"`
	Voice          string   `json:"voice"`
	ResponseFormat string   `json:"response_format"`
	Speed          *float64 `json:"speed,omitempty"`
}

// audioBudget bounds the request body: speech input is prose, not a document.
const maxSpeechInput = 1 << 20

// ttsFormats maps an OpenAI response_format to (backend format, Content-Type)
// for each backend kind. Unknown formats are refused rather than silently
// substituted, so a caller never receives a container it did not ask for.
var ttsFormats = map[string]map[string][2]string{
	"elevenlabs": {
		"mp3":  {"mp3_44100_128", "audio/mpeg"},
		"opus": {"opus_48000_64", "audio/ogg"},
		"pcm":  {"pcm_24000", "audio/pcm"},
	},
	"openai": {
		"mp3":  {"mp3", "audio/mpeg"},
		"opus": {"opus", "audio/ogg"},
		"aac":  {"aac", "audio/aac"},
		"flac": {"flac", "audio/flac"},
		"wav":  {"wav", "audio/wav"},
		"pcm":  {"pcm", "audio/pcm"},
	},
}

// resolveSpeechFormat returns the backend wire format and the Content-Type to
// send the client. An empty request format means mp3.
func resolveSpeechFormat(kind, requested string) (string, string, error) {
	if kind == "" {
		kind = "openai"
	}
	f := strings.ToLower(strings.TrimSpace(requested))
	if f == "" {
		f = "mp3"
	}
	formats, ok := ttsFormats[kind]
	if !ok {
		return "", "", fmt.Errorf("backend kind %q does not support speech", kind)
	}
	pair, ok := formats[f]
	if !ok {
		return "", "", fmt.Errorf("response_format %q is not supported by a %q backend (mp3, opus, pcm)", f, kind)
	}
	return pair[0], pair[1], nil
}

// voiceFor maps a caller-facing voice name (an OpenAI voice like "alloy", or a
// backend's own id) to the backend voice id. An unmapped name passes through
// unchanged when it is long enough to be a vendor id, so callers can address
// backend voices directly without a config edit.
func (c Config) voiceFor(name string) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		if c.DefaultVoice != "" {
			return c.DefaultVoice, nil
		}
		return "", fmt.Errorf("no voice given and no default voice configured")
	}
	if mapped, ok := c.Voices[n]; ok {
		return mapped, nil
	}
	if len(n) >= 16 && !strings.ContainsAny(n, " \t") {
		return n, nil // already a vendor voice id
	}
	if c.DefaultVoice != "" {
		return c.DefaultVoice, nil
	}
	return "", fmt.Errorf("unknown voice %q and no default voice configured", n)
}

// audioRoute resolves a speech model name to its backend and upstream voice
// model. Unknown names fall back to the first configured audio route, so a
// caller that omits "model" still reaches a working voice.
func (c Config) audioRoute(name string) (Backend, string, string, error) {
	n := strings.TrimSpace(name)
	routeName := n
	var r Route
	var ok bool
	if n != "" {
		r, ok = c.AudioRoutes[n]
	}
	if !ok {
		for k, v := range c.AudioRoutes {
			routeName, r, ok = k, v, true
			break
		}
	}
	if !ok {
		return Backend{}, "", "", fmt.Errorf("no speech routes configured")
	}
	b, ok := c.Backends[r.Backend]
	if !ok {
		return Backend{}, "", "", fmt.Errorf("route %q points at unknown backend %q", routeName, r.Backend)
	}
	return b, r.Upstream, routeName, nil
}

// speechUpstream builds the backend request for one synthesis attempt.
func speechUpstream(b Backend, upstreamModel, voiceID, input, format string) (*http.Request, string, error) {
	switch b.Kind {
	case "", "openai":
		body, err := json.Marshal(map[string]any{
			"model":           upstreamModel,
			"input":           input,
			"voice":           voiceID,
			"response_format": format,
		})
		if err != nil {
			return nil, "", err
		}
		req, err := upstreamReq(b, "POST", "/v1/audio/speech", body)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Accept", "audio/*")
		return req, "openai", nil

	case "elevenlabs":
		body, err := json.Marshal(map[string]any{"text": input, "model_id": upstreamModel})
		if err != nil {
			return nil, "", err
		}
		endpoint := b.BaseURL + "/v1/text-to-speech/" + url.PathEscape(voiceID) +
			"?output_format=" + url.QueryEscape(format)
		req, err := http.NewRequest("POST", endpoint, strings.NewReader(string(body)))
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Content-Type", "application/json")
		if b.APIKey != "" {
			req.Header.Set("xi-api-key", b.APIKey)
		}
		req.Header.Set("Accept", "audio/*")
		return req, "elevenlabs", nil
	}
	return nil, "", fmt.Errorf("backend %q has unknown kind %q", b.Name, b.Kind)
}

// doSpeech performs one synthesis attempt. Transient failures are returned as
// errors so the caller can try a fallback route before any byte reaches the
// client.
func doSpeech(b Backend, upstreamModel, voiceID, input, format string) (*http.Response, error) {
	req, _, err := speechUpstream(b, upstreamModel, voiceID, input, format)
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
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	return resp, nil
}

// handleOpenAISpeech serves POST /v1/audio/speech.
func handleOpenAISpeech(w http.ResponseWriter, r *http.Request, cfg Config) {
	app := appFrom(r)
	start := time.Now()

	var req SpeechRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSpeechInput)).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	input := strings.TrimSpace(req.Input)
	if input == "" {
		writeJSON(w, 400, map[string]string{"error": "input is required"})
		return
	}

	backend, upstreamModel, routeName, err := cfg.audioRoute(req.Model)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	voiceID, err := cfg.voiceFor(req.Voice)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	format, contentType, err := resolveSpeechFormat(backend.Kind, req.ResponseFormat)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	resp, lastUpstream, usedRoute, err := attemptSpeech(cfg, app, routeName, req.ResponseFormat, backend, upstreamModel, voiceID, input, format)
	if err != nil {
		status := 502
		if ue, ok := err.(*upstreamError); ok && ue.status > 0 {
			status = ue.status
		}
		metrics.Record(req.Model, lastUpstream, app, "/v1/audio/speech", status, 0, 0, time.Since(start), true)
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", contentType)
	if n := resp.Header.Get("Content-Length"); n != "" {
		w.Header().Set("Content-Length", n)
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, resp.Body); err != nil {
		metrics.Record(req.Model, lastUpstream, app, "/v1/audio/speech", 499, 0, 0, time.Since(start), true)
		return
	}
	chars := len([]rune(input))
	metrics.RecordAudio(usedRoute, lastUpstream, app, "/v1/audio/speech", 200, chars,
		float64(chars)*cfg.TTSCostPerChar, time.Since(start), false)
}

// attemptSpeech tries the requested route, then the configured fallback route
// if the first attempt failed before any audio was sent. Chars are billed once,
// against whichever route actually answered.
func attemptSpeech(cfg Config, app, routeName, requestedFormat string, backend Backend, upstreamModel, voiceID, input, format string) (*http.Response, string, string, error) {
	resp, err := doSpeech(backend, upstreamModel, voiceID, input, format)
	if err == nil {
		return resp, upstreamModel, routeName, nil
	}
	fbName, ok := cfg.Fallbacks[routeName]
	if !ok || fbName == "" || fbName == routeName {
		return nil, upstreamModel, routeName, err
	}
	fbBackend, fbUpstream, _, fbErr := cfg.audioRoute(fbName)
	if fbErr != nil {
		return nil, upstreamModel, routeName, err
	}
	fbFormat, _, fmtErr := resolveSpeechFormat(fbBackend.Kind, requestedFormat)
	if fmtErr != nil {
		return nil, upstreamModel, routeName, err
	}
	resp, err2 := doSpeech(fbBackend, fbUpstream, voiceID, input, fbFormat)
	if err2 != nil {
		return nil, fbUpstream, routeName, err2
	}
	metrics.RecordFallbacks(routeName, app, 1)
	return resp, fbUpstream, fbName, nil
}
