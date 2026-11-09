package main

import (
	"encoding/json"
	"net/netip"
	"regexp"
)

// piiPatterns are the obvious identifiers Bifrost scrubs from cloud-bound
// requests. Kept conservative: high-precision patterns only, so ordinary
// prompt text is never mangled.
//
// Network addresses deliberately do NOT live here. A shape-matching regex
// cannot tell a LAN address from a public one, and matching on shape alone
// mangles things that only look like addresses -- "2026-11-08 05:31:12.196"
// satisfies a naive IPv6 pattern, which is how every log timestamp in a tool
// result came back as [REDACTED]. Addresses go through redactAddresses, which
// parses them and leaves everything non-public alone.
var piiPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`),                                // email
	regexp.MustCompile(`\b(?:\+?1[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}\b`),                      // US/CA phone
	regexp.MustCompile(`\b\d{4}[- ]?\d{4}[- ]?\d{4}[- ]?\d{4}\b`),                                      // card number
	regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),                                                        // US SSN
	regexp.MustCompile(`\b(?:sk|ssh|ghp|gho|ghu|ghs|github_pat)_[A-Za-z0-9_]{8,}\b`),                   // API/SSH keys
	regexp.MustCompile(`-----BEGIN [A-Z ]+ PRIVATE KEY-----[\s\S]*?-----END [A-Z ]+ PRIVATE KEY-----`), // PEM private key
}

var (
	ipv4Candidate = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	// Broad on purpose: parsing is the filter. Anything that does not parse as
	// an address is left exactly as it was.
	ipv6Candidate = regexp.MustCompile(`[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,}`)
	cgnat         = netip.PrefixFrom(netip.AddrFrom4([4]byte{100, 64, 0, 0}), 10)
)

// redact increments: pattern pass first, then the address pass.
func redact(s string) string {
	for _, re := range piiPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return redactAddresses(s)
}

// redactAddresses scrubs only publicly routable addresses. RFC1918, CGNAT,
// loopback, link-local and unspecified all stay readable: this runs on a LAN
// where those are the working vocabulary, not a secret.
func redactAddresses(s string) string {
	s = ipv4Candidate.ReplaceAllStringFunc(s, func(m string) string {
		if addr, err := netip.ParseAddr(m); err == nil && addr.Is4() && addressIsPublic(addr) {
			return "[REDACTED]"
		}
		return m
	})
	s = ipv6Candidate.ReplaceAllStringFunc(s, func(m string) string {
		if addr, err := netip.ParseAddr(m); err == nil && addr.Is6() && addressIsPublic(addr) {
			return "[REDACTED]"
		}
		return m
	})
	return s
}

func addressIsPublic(a netip.Addr) bool {
	if a.Unmap().IsLoopback() || a.Unmap().IsPrivate() || a.Unmap().IsLinkLocalUnicast() ||
		a.Unmap().IsLinkLocalMulticast() || a.Unmap().IsUnspecified() {
		return false
	}
	return !cgnat.Contains(a.Unmap())
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
