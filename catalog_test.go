package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrivacyTier(t *testing.T) {
	cases := []struct {
		name  string
		local bool
		want  string
	}{
		{"private:local", true, "private"},
		{"coach", false, "cloud"},
		{"my-local-model", true, "local"},
	}
	for _, c := range cases {
		if got := privacyTier(c.name, c.local); got != c.want {
			t.Errorf("privacyTier(%q, %v) = %q, want %q", c.name, c.local, got, c.want)
		}
	}
}

func TestCapabilities(t *testing.T) {
	if got := capabilities("embed"); len(got) != 1 || got[0] != "embeddings" {
		t.Errorf("capabilities(embed) = %v", got)
	}
	if got := capabilities("coach"); len(got) != 2 || got[0] != "chat" || got[1] != "completions" {
		t.Errorf("capabilities(coach) = %v", got)
	}
}

func TestModelCatalog(t *testing.T) {
	cfg := Config{
		Backends: map[string]Backend{
			"deepseek": {Name: "deepseek", BaseURL: "https://api.deepseek.com"},
			"epsilon":  {Name: "epsilon", BaseURL: "http://192.168.0.105:11434", Local: true},
		},
		Routes: map[string]Route{
			"coach":         {Name: "coach", Backend: "deepseek", Upstream: "deepseek-v4-flash"},
			"private:local": {Name: "private:local", Backend: "epsilon", Upstream: "qwen2.5:7b"},
			"embed":         {Name: "embed", Backend: "epsilon", Upstream: "nomic-embed-text"},
		},
	}

	byName := map[string]modelMeta{}
	for _, m := range cfg.modelCatalog() {
		byName[m.ID] = m
	}

	if m := byName["coach"]; m.Privacy != "cloud" || m.Backend != "deepseek" || m.Upstream != "deepseek-v4-flash" {
		t.Errorf("coach = %+v", m)
	}
	if m := byName["coach"]; m.Pricing.InputPer1M != 0.22 || m.Pricing.OutputPer1M != 0.66 {
		t.Errorf("coach pricing = %+v", m.Pricing)
	}
	if m := byName["private:local"]; m.Privacy != "private" || m.Backend != "epsilon" || m.Pricing.InputPer1M != 0 {
		t.Errorf("private:local = %+v", m)
	}
	if m := byName["embed"]; len(m.Capabilities) != 1 || m.Capabilities[0] != "embeddings" {
		t.Errorf("embed capabilities = %+v", m.Capabilities)
	}
	if _, ok := byName["nope"]; ok {
		t.Error("unexpected model in catalog")
	}
}

func TestHandleOpenAIModels(t *testing.T) {
	cfg := Config{
		Backends: map[string]Backend{"deepseek": {Name: "deepseek"}},
		Routes:   map[string]Route{"coach": {Name: "coach", Backend: "deepseek", Upstream: "deepseek-v4-flash"}},
	}
	rec := httptest.NewRecorder()
	handleOpenAIModels(rec, cfg)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"privacy":"cloud"`) || !strings.Contains(rec.Body.String(), `"id":"coach"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestHandleOpenAIModelDetail(t *testing.T) {
	cfg := Config{
		Backends: map[string]Backend{"deepseek": {Name: "deepseek"}},
		Routes:   map[string]Route{"coach": {Name: "coach", Backend: "deepseek", Upstream: "deepseek-v4-flash"}},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models/coach", nil)
	req.SetPathValue("name", "coach")
	handleOpenAIModelDetail(rec, req, cfg)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"upstream":"deepseek-v4-flash"`) {
		t.Errorf("detail status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/models/nope", nil)
	req.SetPathValue("name", "nope")
	handleOpenAIModelDetail(rec, req, cfg)
	if rec.Code != 404 {
		t.Errorf("missing model status=%d, want 404", rec.Code)
	}
}

// A model priced through the PRICING env must appear with that price in the
// catalog. defaultPricing() alone made every added backend look free on
// /v1/models while the ledger charged for it.
func TestCatalogUsesConfiguredPricing(t *testing.T) {
	t.Setenv("PRICING", `{"claude-sonnet-5-5":{"input":2,"output":10}}`)
	cfg := Config{
		Backends: map[string]Backend{"anthropic": {Name: "anthropic", BaseURL: "https://api.anthropic.com", Kind: "anthropic"}},
		Routes: map[string]Route{
			"claude": {Name: "claude", Backend: "anthropic", Upstream: "claude-sonnet-5-5"},
		},
	}
	var found bool
	for _, m := range cfg.modelCatalog() {
		if m.ID != "claude" {
			continue
		}
		found = true
		if m.Pricing.InputPer1M != 2 || m.Pricing.OutputPer1M != 10 {
			t.Fatalf("pricing = %+v, want 2/10 from PRICING", m.Pricing)
		}
	}
	if !found {
		t.Fatal("claude not in catalog")
	}
}
