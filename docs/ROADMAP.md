# Bifrost Roadmap

## Vision

Bifrost is the homelab's **LLM control plane** — the one door every app and agent
knocks on, which decides *which brain* to route to (cloud or local), keeps private
data private, and tells you what it's all costing.

It is the AI mirror of what Heimdall already is at the network edge. Right now
Bifrost is a thin shim that makes DeepSeek look like a local Ollama; the roadmap
below turns it into a bridge with a real decision layer.

## Current state

- **Routing (v0.4):** multi-backend registry + routing table (`BACKENDS` +
  `ROUTES`), with a `private:` prefix that refuses to route to cloud backends.
- Ollama-native (`/api/tags`, `/api/chat`, `/api/generate`) **and** OpenAI
  (`/v1/models`, `/v1/chat/completions`) APIs, so any client works.
- Model aliasing (`coach` → `deepseek-v4-flash`).
- **Privacy (v0.5):** PII redaction (emails, phones, cards, SSNs, IPs, keys) on
  all cloud-bound traffic; local backends skip it.
- **Observability (v0.2–v0.3):** Prometheus `/metrics` — request counts, token in/out,
  estimated cost (cache-miss rates, peak/off-peak aware), latency histogram, and
  error counts, labeled by model and app (`X-Bifrost-App` header) — plus a bespoke
  live HTML dashboard at `/` (zero-JS, server-rendered).
- **Governance (v0.7):** hard monthly budgets (global `BUDGET` + per-app
  `APP_BUDGETS`), per-app rate limits (`RATE_LIMIT`), and optional per-app bearer
  auth (`APP_KEYS`) — backed by a durable JSONL spend ledger (`LEDGER_FILE`) so
  budgets survive restarts. Over budget → `402`, rate-limited → `429`,
  unauthenticated → `401`.
- **Caching (v0.8):** exact-match response caching — identical requests (retries,
  idempotent re-sends) skip the upstream call entirely (zero cost, zero latency).
  Bounded LRU + TTL, in-memory only; hit-rate surfaced on the dashboard.
- **Embeddings (v0.9):** Ollama-native + OpenAI-compatible embeddings endpoints,
  routed to any backend holding an embedding model (e.g. Epsilon's
  `nomic-embed-text`). Upstream calls standardized on `/v1/*` paths.
- **Circuit breaker (v0.10):** per-backend breaker — after `CIRCUIT_THRESHOLD`
  consecutive failures, fail fast (503) instead of hanging on a downed backend;
  a half-open probe after `CIRCUIT_COOLDOWN` re-closes on recovery. Makes the
  nomadic local backend degrade gracefully while it's offline.
- **Transparent failover (v0.11):** retry + failover now apply to every path —
  streaming, `/v1` passthrough, and embeddings — not just non-streaming chat.
  A unified `resolve` loop retries transient failures and walks `FALLBACKS`
  before anything reaches the client; streaming retries up to the first flushed
  byte. Local backends dial with a 3s timeout so a downed worker fails over in
  seconds. The client sees one slow success, never an internal failure.
- **Proactive health polling (v0.12):** a poller probes every backend on
  `HEALTH_INTERVAL` and pre-opens the circuit while a backend is unreachable, so
  the first request after an outage fails over instantly instead of paying the
  retry cost. On recovery it half-opens to re-admit traffic. Exposed as
  `bifrost_backend_healthy` per backend.
- **Semantic caching (v0.13):** opt-in near-neighbor cache — prompts are
  embedded via the local embedding brain and a *similar* cached response is
  served when cosine similarity clears `SEMANTIC_THRESHOLD`. First real use of
  the local embeddings: a reworded question hits cache instead of upstream.
  Degrades gracefully when the embedding backend is down.
- Single static binary, stdlib-only, deployed on Hyperion at `bifrost.lab:11434`;
  the only state is an optional spend ledger file.

## Initiatives (priority order)

### 1. Multi-backend routing — ✅ shipped (v0.4)

`BACKENDS` (JSON array of backends with base URL + key) + `ROUTES` (client-model →
`backend/upstream-model`). Unknown names fall back to the default backend
unchanged. `coach` → DeepSeek today; `private:*` → Thoth becomes a one-line
`ROUTES` entry the moment a local backend exists. Legacy `UPSTREAM_BASE_URL` /
`MODELS` still work as a single default backend.

### 2. Usage & cost observability — ✅ shipped (v0.2 + v0.3)

`/metrics` (Prometheus format) plus a bespoke live HTML dashboard at `/`:
cumulative cost/tokens/latency/errors per model+app and a rolling 100-request
window. **Decision:** built a bespoke dashboard rather than standing up
Prometheus+Grafana — the homelab runs Beszel, not a metrics stack, and two new
services to visualize one endpoint was the tail wagging the dog. `/metrics` stays
as the Prometheus hook: if cross-service history/alerting is ever wanted,
Prometheus can scrape Bifrost with zero rework on Bifrost's side.

### 3. Embeddings endpoint — ✅ shipped (v0.9)

`/api/embeddings` + `/api/embed` (Ollama-native) and `/v1/embeddings`
(OpenAI-compatible), routed to whichever backend holds an embedding model (a
`ROUTES` entry like `embed → epsilon/nomic-embed-text`). Bifrost translates
Ollama↔OpenAI embedding shapes. Also standardized all upstream calls on the real
OpenAI paths (`/v1/chat/completions`, `/v1/embeddings`) so Ollama, vLLM, and
DeepSeek are all first-class backends instead of DeepSeek's `/chat/completions`
shorthand.

### 4. Privacy routing + PII redaction — ✅ shipped (v0.5)

`private:*` models refuse to route to a non-local backend (a hard `400`, not a
silent leak). All cloud-bound traffic has obvious PII scrubbed before it leaves
the LAN — emails, phones, card numbers, SSNs, IPs, API/SSH keys, PEM private
keys. Local backends are auto-detected (loopback/RFC1918/`.lab`) and skip
redaction.

### 5. Caching — ✅ exact-match shipped (v0.8); semantic ⬜

Exact-match response caching: identical requests (retries, idempotent agent
re-sends) skip the upstream call entirely — zero cost, zero latency. Bounded LRU
with TTL (`CACHE_TTL`/`CACHE_MAX`), in-memory only (never persisted — responses
can echo request PII), non-streaming only (like retry/fallback).
`bifrost_cache_hits_total` + `bifrost_cache_misses_total`, plus a hit-rate on the
dashboard. Semantic caching still builds on #3 (embeddings) and remains future
work.

### 6. Resilience: retry + fallback — ✅ shipped (v0.6)

Non-streaming completions retry transient failures (network, `429`, `5xx`) with
exponential backoff (`RETRIES`, default 2) and fall back through a model chain
(`FALLBACKS`, e.g. `{"deepseek-v4-pro":"deepseek-v4-flash"}`) when the primary
fails for good. `bifrost_retries_total` + `bifrost_fallbacks_total` counters.
Streaming and `/v1` passthrough stay single-attempt (retrying a half-flushed
stream would corrupt the client). JSON-output repair remains a future increment.

### 7. Governance: budgets, rate limits, auth — ✅ shipped (v0.7)

Hard monthly budgets (global `BUDGET` + per-app `APP_BUDGETS`), per-app rate
limits (`RATE_LIMIT`, requests/min), and optional per-app bearer auth (`APP_KEYS`,
`app→key`). Spend is tracked in a durable JSONL ledger (`LEDGER_FILE`) so budgets
survive restarts. Over budget → `402`, rate-limited → `429`, unauthenticated →
`401`. Without `APP_KEYS`, the `X-Bifrost-App` header still works (spoofable —
deploy keys when per-app budgets need to be trusted).

### 8. Model catalog with metadata ⬜

`/v1/models` returns not just names but capabilities, cost, latency, and privacy
tier — so an app can ask "what's available and what's right for me" instead of
hardcoding a model.

### 9. Retrieval endpoint ⬜ *(stretch)*

`/v1/retrieval` — embeddings + vector search over the vault, so agents get
semantic search without each reimplementing it.

## Non-goals

- **A model runner** — vLLM/llama.cpp already own that.
- **A training pipeline.**
- **A full vector database** — delegate to an existing store rather than embed one.

Bifrost stays thin: its value is the *decision layer*, not the compute.
