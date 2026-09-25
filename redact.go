package main

import (
	"encoding/json"
	"regexp"
)

// piiPatterns are the obvious identifiers Bifrost scrubs from cloud-bound
// requests. Kept conservative: high-precision patterns only, so ordinary
// prompt text is never mangled.
var piiPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`),                                // email
	regexp.MustCompile(`\b(?:\+?1[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}\b`),                      // US/CA phone
	regexp.MustCompile(`\b\d{4}[- ]?\d{4}[- ]?\d{4}[- ]?\d{4}\b`),                                      // card number
	regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),                                                        // US SSN
	regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`),                                                  // IPv4
	regexp.MustCompile(`\b(?:[0-9A-Fa-f]{1,4}:){2,7}[0-9A-Fa-f]{1,4}\b`),                               // IPv6
	regexp.MustCompile(`\b(?:sk|ssh|ghp|gho|ghu|ghs|github_pat)_[A-Za-z0-9_]{8,}\b`),                   // API/SSH keys
	regexp.MustCompile(`-----BEGIN [A-Z ]+ PRIVATE KEY-----[\s\S]*?-----END [A-Z ]+ PRIVATE KEY-----`), // PEM private key
}

// redact scrubs recognized PII patterns from a single string.
func redact(s string) string {
	for _, re := range piiPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}

// maybeRedact rewrites a JSON request body with every string value redacted,
// unless the backend is local (traffic already stays on the LAN). Non-JSON
// bodies are returned unchanged.
func maybeRedact(b Backend, body []byte) []byte {
	if b.Local {
		return body
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	v = redactValue(v)
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return out
}

// redactValue recurses through a decoded JSON value, redacting every string.
func redactValue(v any) any {
	switch t := v.(type) {
	case string:
		return redact(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = redactValue(e)
		}
		return out
	default:
		return v
	}
}
