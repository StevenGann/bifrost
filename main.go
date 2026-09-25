package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
)

// Backend is an upstream LLM endpoint Bifrost can route to.
type Backend struct {
	Name    string
	BaseURL string
	APIKey  string
	Local   bool // true when the backend is on the LAN (skip PII redaction)
}

// Route maps a client-facing model name to a backend + upstream model name.
type Route struct {
	Name     string
	Backend  string
	Upstream string
}

type Config struct {
	Port     string
	Backends map[string]Backend
	Default  string
	Routes   map[string]Route
}

// metrics is the package-level collector. It is initialized to a working
// default here so tests never hit a nil receiver; main() replaces it with
// env-configured pricing.
var metrics = newMetrics(defaultPricing())

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// normalizeBase trims trailing slashes and a trailing /v1 suffix.
func normalizeBase(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	base = strings.TrimSuffix(base, "/v1")
	return base
}

// isLocalHost reports whether a base URL points at the LAN (loopback, RFC1918,
// or a private TLD), so Bifrost knows it can skip PII redaction for it.
func isLocalHost(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if h == "localhost" || h == "::1" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	for _, suf := range []string{".lab", ".local", ".internal", ".lan", ".home"} {
		if strings.HasSuffix(h, suf) {
			return true
		}
	}
	return false
}

// backendConfig is the JSON shape of the BACKENDS env.
type backendConfig struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
	Local     *bool  `json:"local"`
}

// backendIsLocal resolves a backend's local-ness: explicit `local` field wins,
// otherwise it is inferred from the base URL host.
func backendIsLocal(bc backendConfig) bool {
	if bc.Local != nil {
		return *bc.Local
	}
	return isLocalHost(normalizeBase(bc.BaseURL))
}

// splitRoute splits a route target "backend/model" into its parts.
func splitRoute(target string) (backend, model string) {
	if i := strings.Index(target, "/"); i >= 0 {
		return strings.TrimSpace(target[:i]), strings.TrimSpace(target[i+1:])
	}
	return "", strings.TrimSpace(target)
}

// parseModels parses the legacy MODELS env "a,b=upstream,c" into routes bound to
// the given backend.
func parseModels(s, backend string) []Route {
	var out []Route
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, up := part, part
		if i := strings.Index(part, "="); i >= 0 {
			name = strings.TrimSpace(part[:i])
			up = strings.TrimSpace(part[i+1:])
		}
		out = append(out, Route{Name: name, Backend: backend, Upstream: up})
	}
	return out
}

func loadConfig() Config {
	cfg := Config{
		Port:     envOr("PORT", "11434"),
		Backends: map[string]Backend{},
		Routes:   map[string]Route{},
	}

	// Multi-backend config: BACKENDS=[...] + ROUTES={"client":"backend/model"}.
	if raw := os.Getenv("BACKENDS"); raw != "" {
		var bcs []backendConfig
		if err := json.Unmarshal([]byte(raw), &bcs); err != nil {
			log.Printf("WARNING: ignoring malformed BACKENDS: %v", err)
		}
		for i, bc := range bcs {
			if bc.Name == "" || bc.BaseURL == "" {
				continue
			}
			cfg.Backends[bc.Name] = Backend{Name: bc.Name, BaseURL: normalizeBase(bc.BaseURL), APIKey: os.Getenv(bc.APIKeyEnv), Local: backendIsLocal(bc)}
			if i == 0 {
				cfg.Default = bc.Name
			}
		}
		if raw := os.Getenv("ROUTES"); raw != "" {
			var m map[string]string
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				log.Printf("WARNING: ignoring malformed ROUTES: %v", err)
			}
			for name, target := range m {
				backend, up := splitRoute(target)
				if backend == "" {
					backend = cfg.Default
				}
				cfg.Routes[name] = Route{Name: name, Backend: backend, Upstream: up}
			}
		}
	}

	// Legacy single-upstream fallback (backward compatible).
	if len(cfg.Backends) == 0 {
		base := normalizeBase(os.Getenv("UPSTREAM_BASE_URL"))
		if base == "" {
			base = "https://api.deepseek.com"
		}
		cfg.Backends["default"] = Backend{Name: "default", BaseURL: base, APIKey: os.Getenv("UPSTREAM_API_KEY"), Local: isLocalHost(base)}
		cfg.Default = "default"
		for _, r := range parseModels(os.Getenv("MODELS"), "default") {
			cfg.Routes[r.Name] = r
		}
		if len(cfg.Routes) == 0 {
			cfg.Routes["deepseek-v4-flash"] = Route{Name: "deepseek-v4-flash", Backend: "default", Upstream: "deepseek-v4-flash"}
		}
	}

	return cfg
}

// route resolves a requested model name to its backend + upstream model name.
// Unknown names fall back to the default backend, passed through unchanged.
// A `private:`-prefixed name must resolve to a local backend; otherwise the
// request is refused rather than leaking to a cloud upstream.
func (c Config) route(name string) (Backend, string, error) {
	wantsLocal := strings.HasPrefix(name, "private:")
	if r, ok := c.Routes[name]; ok {
		if b, ok := c.Backends[r.Backend]; ok {
			if wantsLocal && !b.Local {
				return Backend{}, "", fmt.Errorf("model %q requires a local backend but %q is not local", name, r.Backend)
			}
			return b, r.Upstream, nil
		}
	}
	b := c.defaultBackend()
	if wantsLocal && !b.Local {
		return Backend{}, "", fmt.Errorf("model %q requires a local backend but none is configured", name)
	}
	return b, name, nil
}

func (c Config) defaultBackend() Backend {
	if b, ok := c.Backends[c.Default]; ok {
		return b
	}
	for _, b := range c.Backends {
		return b
	}
	return Backend{}
}

// modelNames returns the client-facing model names, sorted.
func (c Config) modelNames() []string {
	names := make([]string, 0, len(c.Routes))
	for n := range c.Routes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// appFrom identifies the calling app for metrics from the X-Bifrost-App header,
// falling back to "unknown".
func appFrom(r *http.Request) string {
	if a := strings.TrimSpace(r.Header.Get("X-Bifrost-App")); a != "" {
		return a
	}
	return "unknown"
}

// loadPricing merges env PRICING (JSON: {"model":{"input":x,"output":y}}) over
// the built-in DeepSeek defaults.
func loadPricing() map[string]pricing {
	p := defaultPricing()
	if raw := os.Getenv("PRICING"); raw != "" {
		var extra map[string]pricing
		if err := json.Unmarshal([]byte(raw), &extra); err != nil {
			log.Printf("WARNING: ignoring malformed PRICING: %v", err)
		} else {
			for k, v := range extra {
				p[k] = v
			}
		}
	}
	return p
}

func main() {
	cfg := loadConfig()
	metrics = newMetrics(loadPricing())

	mux := http.NewServeMux()

	// Ollama-native API
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, r *http.Request) { handleTags(w, cfg) })
	mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) { handleChat(w, r, cfg) })
	mux.HandleFunc("POST /api/generate", func(w http.ResponseWriter, r *http.Request) { handleGenerate(w, r, cfg) })
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"version": "0.1.0-bifrost"})
	})
	mux.HandleFunc("POST /api/embeddings", notSupported)
	mux.HandleFunc("POST /api/embed", notSupported)

	// OpenAI-compatible API (same as Ollama's /v1)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) { handleOpenAIModels(w, cfg) })
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { handleOpenAIChat(w, r, cfg) })

	// Ops
	mux.Handle("GET /{$}", metrics.Dashboard())
	mux.Handle("GET /dashboard", metrics.Dashboard())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.Handle("GET /metrics", metrics.Handler())

	addr := ":" + cfg.Port
	log.Printf("bifrost listening on %s  backends=%d  routes=%d", addr, len(cfg.Backends), len(cfg.Routes))
	log.Fatal(http.ListenAndServe(addr, mux))
}

func notSupported(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 501, map[string]string{"error": "embeddings are not supported by the configured upstream"})
}
