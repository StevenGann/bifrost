package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
)

// ModelAlias maps a client-facing model name to the upstream model name.
// e.g. Name="coach", Upstream="deepseek-v4-flash"
type ModelAlias struct {
	Name     string
	Upstream string
}

type Config struct {
	Port         string
	UpstreamBase string
	UpstreamKey  string
	Models       []ModelAlias
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseModels parses "a,b=upstream,c" into aliases.
func parseModels(s string) []ModelAlias {
	var out []ModelAlias
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.Index(part, "="); i >= 0 {
			out = append(out, ModelAlias{
				Name:     strings.TrimSpace(part[:i]),
				Upstream: strings.TrimSpace(part[i+1:]),
			})
		} else {
			out = append(out, ModelAlias{Name: part, Upstream: part})
		}
	}
	return out
}

func loadConfig() Config {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("UPSTREAM_BASE_URL")), "/")
	base = strings.TrimSuffix(base, "/v1") // normalize OpenAI-style base URLs
	if base == "" {
		base = "https://api.deepseek.com"
	}
	models := parseModels(os.Getenv("MODELS"))
	if len(models) == 0 {
		models = []ModelAlias{{Name: "deepseek-v4-flash", Upstream: "deepseek-v4-flash"}}
	}
	return Config{
		Port:         envOr("PORT", "11434"),
		UpstreamBase: base,
		UpstreamKey:  os.Getenv("UPSTREAM_API_KEY"),
		Models:       models,
	}
}

// upstreamModel maps a requested name to the upstream name, passing unknown
// names through unchanged so a single-model backend still works.
func (c Config) upstreamModel(name string) string {
	for _, m := range c.Models {
		if m.Name == name {
			return m.Upstream
		}
	}
	return name
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	cfg := loadConfig()
	if cfg.UpstreamKey == "" {
		log.Println("WARNING: UPSTREAM_API_KEY is empty — upstream calls will be unauthenticated")
	}

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

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	addr := ":" + cfg.Port
	log.Printf("bifrost listening on %s  upstream=%s  models=%d", addr, cfg.UpstreamBase, len(cfg.Models))
	log.Fatal(http.ListenAndServe(addr, mux))
}

func notSupported(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 501, map[string]string{"error": "embeddings are not supported by the configured upstream"})
}
