# Bifrost Roadmap

## Vision

Bifrost is the homelab's **LLM control plane** — the one door every app and agent
knocks on, which decides *which brain* to route to (cloud or local), keeps private
data private, and tells you what it's all costing.

It is the AI mirror of what Heimdall already is at the network edge. Right now
Bifrost is a thin shim that makes DeepSeek look like a local Ollama; the roadmap
below turns it into a bridge with a real decision layer.

## Current state

- Single upstream (DeepSeek, OpenAI-compatible), configured via env.
- Ollama-native (`/api/tags`, `/api/chat`, `/api/generate`) **and** OpenAI
  (`/v1/models`, `/v1/chat/completions`) APIs, so any client works.
- Model aliasing (`coach` → `deepseek-v4-flash`).
- **Observability (v0.2–v0.3):** Prometheus `/metrics` — request counts, token in/out,
  estimated cost (cache-miss rates, peak/off-peak aware), latency histogram, and
  error counts, labeled by model and app (`X-Bifrost-App` header) — plus a bespoke
  live HTML dashboard at `/` (zero-JS, server-rendered).
- Stateless, single static binary, stdlib-only, deployed on Hyperion at
  `bifrost.lab:11434`.

## Initiatives (priority order)

### 1. Multi-backend routing — *the "bridge" part* ⬜

Generalize the single upstream into a **routing table**: `coach` → DeepSeek-flash,
`private:*` → a local model on Thoth (2× RTX 6000 Ada ≈ 96 GB VRAM), `embedding:*`
→ a local embedding model. Same API to every consumer; the *routing* is Bifrost's
job. This is what makes Thoth/Epsilon instantly useful the moment they power on.

### 2. Usage & cost observability — ✅ shipped (v0.2 + v0.3)

`/metrics` (Prometheus format) plus a bespoke live HTML dashboard at `/`:
cumulative cost/tokens/latency/errors per model+app and a rolling 100-request
window. **Decision:** built a bespoke dashboard rather than standing up
Prometheus+Grafana — the homelab runs Beszel, not a metrics stack, and two new
services to visualize one endpoint was the tail wagging the dog. `/metrics` stays
as the Prometheus hook: if cross-service history/alerting is ever wanted,
Prometheus can scrape Bifrost with zero rework on Bifrost's side.

### 3. Embeddings endpoint ⬜

DeepSeek has no embeddings API; several apps need one (Subwave currently pays
Gemini for embeddings, semantic search over the Caldera vault, future RAG). Serve
`/v1/embeddings` + `/api/embeddings` backed by a local model, collapsing all of
that into one endpoint and dropping a provider.

### 4. Privacy routing + PII redaction ⬜

A policy layer: requests tagged sensitive route to local models instead of leaving
the house, and a redaction pass strips PII (names, addresses, credentials) from
anything that *does* go to a third-party backend. "Sensitive stuff never leaves
the LAN" as a first-class, configurable rule.

### 5. Caching ⬜

Exact + semantic prompt caching. Agents re-send a lot of near-identical context
every turn; caching that saves tokens and latency. Semantic caching builds on #3
(needs embeddings) and could cut 20–40% off agent-traffic cost.

### 6. Resilience: fallback + retry + JSON enforcement ⬜

Circuit-breaker to a fallback model/provider when the primary hiccups or
rate-limits; auto-retry; and "model returned invalid JSON → nudge it to fix it".
Every consumer gets more robust for free.

### 7. Model catalog with metadata ⬜

`/v1/models` returns not just names but capabilities, cost, latency, and privacy
tier — so an app can ask "what's available and what's right for me" instead of
hardcoding a model.

### 8. Retrieval endpoint ⬜ *(stretch)*

`/v1/retrieval` — embeddings + vector search over the vault, so agents get
semantic search without each reimplementing it.

## Non-goals

- **A model runner** — vLLM/llama.cpp already own that.
- **A training pipeline.**
- **A full vector database** — delegate to an existing store rather than embed one.

Bifrost stays thin: its value is the *decision layer*, not the compute.
