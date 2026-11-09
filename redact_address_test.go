package main

import (
	"fmt"
	"strings"
	"testing"
)

// The address pass exists because a shape-matching regex cannot tell a LAN
// address from a public one, and matching on shape alone mangles anything that
// merely looks like an address.
//
// Addresses are assembled with Sprintf rather than written as literals so this
// file survives a text pipeline that rewrites dotted quads.
func TestRedactAddressScope(t *testing.T) {
	lan := fmt.Sprintf("%d.%d.%d.%d", 192, 168, 10, 5)
	lan2 := fmt.Sprintf("%d.%d.%d.%d", 10, 4, 2, 9)
	cgnatAddr := fmt.Sprintf("%d.%d.%d.%d", 100, 64, 1, 7)
	loopback := fmt.Sprintf("%d.%d.%d.%d", 127, 0, 0, 1)
	public := fmt.Sprintf("%d.%d.%d.%d", 203, 0, 113, 9)

	cases := []struct{ name, in, want string }{
		{"lan survives", "service at " + lan + ":5030", "service at " + lan + ":5030"},
		{"rfc1918 other block survives", "peer " + lan2 + " up", "peer " + lan2 + " up"},
		{"cgnat survives", "uplink " + cgnatAddr, "uplink " + cgnatAddr},
		{"loopback survives", "bound to " + loopback, "bound to " + loopback},
		{"public is redacted", "lookup " + public + " now", "lookup [REDACTED] now"},
		{"clock time survives", "2026-11-08 05:31:22,196 - core.config - WARNING", "2026-11-08 05:31:22,196 - core.config - WARNING"},
		{"version string survives", "release v2.15.0 / slskd 0.25.1", "release v2.15.0 / slskd 0.25.1"},
		{"email still redacted", "mail me at user@example.com", "mail me at [REDACTED]"},
		{"ipv6 public redacted", "route via 2001:db8::1 today", "route via [REDACTED] today"},
	}

	for _, c := range cases {
		if got := redact(c.in); got != c.want {
			t.Errorf("%s: redact(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// Guard against regressing to a bare-shape IPv6 match: a log timestamp must not
// be mistaken for an address even though it has three colon-separated groups.
func TestRedactDoesNotEatTimestamps(t *testing.T) {
	in := "2026-11-08 05:31:22,196 - services.scan - INFO - done"
	if got := redact(in); strings.Contains(got, "REDACTED") {
		t.Fatalf("timestamp was mangled: %q", got)
	}
}
