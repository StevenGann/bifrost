# Bifrost Roadmap

## Vision

Bifrost is the one door every app and agent knocks on: it decides which model a
request reaches, keeps private data on the network, absorbs the failure of
anything behind it, and reports what each caller spent. The objectives in the
README are the contract; this document tracks how far the implementation has
got.

## Current state

- **Routing (v0.4):** multi-backend registry + routing table (`BACKENDS` +
  `ROUTES`), with a `private:` prefix that refuses to route to cloud backends.
- Ollama-native (`/api/tags`, `/api/chat`, `/api/generate`) **and** OpenAI
  (`/v1/models`, `/v1/chat/completions`) APIs, so any client works.
- Model aliasing (`coach` → `deepseek-v4-flash`).
- **Privacy (v0.5):** PII redaction (emails, phones, cards, SSNs, IPs, keys) on
  all cloud-bound traffic; local backends skip it.
- **Observability (v0.2–v0.3, extended v0.17):** Prometheus `/metrics` — request
  counts, token in/out, estimated cost (cache-miss rates, peak/off-peak aware),
  latency histogram, and error counts, labeled by model and app (`X-Bifrost-App`
  header) — plus a live dashboard at `/dashboard`. The page is drawn
  client-side: the server ships a snapshot inside the HTML for an instant first
  paint, then the page polls `/dashboard.json` every two seconds and patches the
  DOM in place. One renderer, no reloads, no lost scroll position, and a label
  pair per cell so it re-lays-out as cards on a phone.
- **Governance (v0.7):** hard monthly budgets (global `BUDGET` + per-app
  `APP_BUDGETS`), per-app rate limits (`RATE_LIMIT`), and optional per-app bearer
  auth (`APP_KEYS`) — backed by a durable JSONL spend ledger (`LEDGER_FILE`) so
  budgets survive restarts. Over budget → `402`, rate-limited → `429`,
  unauthenticated → `401`.
- **Caching (v0.8):** exact-match response caching — identical requests (retries,
  idempotent re-sends) skip the upstream call entirely (zero cost, zero latency).
  Bounded LRU + TTL, in-memory only; hit-rate surfaced on the dashboard.
- **Embeddings (v0.9):** Ollama-native + OpenAI-compatible embeddings endpoints,
  routed to any backend holding an embedding model (e.g. a local
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
- **Semantic caching (v0.13):** opt-in near-neighbor response cache — prompts
  are embedded via the local embedding brain and a *similar* cached response is
  served when cosine similarity clears `SEMANTIC_THRESHOLD`. First real use of
  the local embeddings: a reworded question hits cache instead of upstream.
  Degrades gracefully when the embedding backend is down.
- **Model catalog (v0.15):** `/v1/models` (and `/v1/models/{name}`) return each
  model's privacy tier, backend, upstream model, capabilities, and per-1M-token
  pricing — agents can discover and pick instead of hardcoding model names.
- **Key pool (v0.16):** a backend can hold many keys; Bifrost rotates across
  them (weighted round-robin) and transparently fails over to the next key on a
  `429`/`401`/`403`, cooling the rate-limited key before backend failover
  applies. Tracked as `bifrost_key_rotations_total`.
- **Speech (v0.17):** `POST /v1/audio/speech` in the OpenAI shape, in front of any
  voice backend — the backend's `kind` selects the wire dialect (`openai` or
  `elevenlabs`). Audio routes live in `AUDIO_ROUTES` so a voice target never
  appears as a chat model in `/v1/models`; `FALLBACKS` covers a dead voice the
  way it covers a dead chat backend. Speech bills per character, so the ledger
  line gained a `chars` field and the dashboard a "chars spoken" total.
- **Tenant keys (v0.18):** Bifrost mints its own client keys into `KEYS_FILE`,
  storing only their SHA-256. A key carries its own monthly budget and rate
  limit, its last-used stamp, and can be revoked without a restart; the admin
  surface is `/admin/keys`, unregistered unless `ADMIN_KEY` is set. Anonymous
  callers are a policy (`ANON` = `open`/`limited`/`closed`), and when limited
  they get a per-source-IP rate limit plus a shared monthly cap. `APP_KEYS`
  imports as static keys, and each ledger line now carries `key` beside `app`.
- **Portability (v0.19):** the binary and the image assume nothing about our
  cluster. `deploy/compose/` ships a compose file and an `.env.example`; a CI job
  (`scripts/portability-test.sh`) starts the image on a fresh volume as an
  unprivileged user, mints a key, restarts, and asserts the key and the month's
  spend survive. `/data` exists in the image owned by the runtime user, the
  image carries a `HEALTHCHECK` that runs `/bifrost -health`, `SIGTERM` drains
  for 10s, and an unusable state directory now fails at startup instead of
  silently under-recording spend.
- Single static binary, stdlib-only; the only state is an optional spend ledger
  file and an optional key registry.

## Initiatives (priority order)

### 1. Multi-backend routing — ✅ shipped (v0.4)

`BACKENDS` (JSON array of backends with base URL + key) + `ROUTES` (client-model →
`backend/upstream-model`). Unknown names fall back to the default backend
unchanged. `coach` → DeepSeek today; a local backend becomes a one-line
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
`ROUTES` entry like `embed → local/nomic-embed-text`). Bifrost translates
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

### 5. Caching — ✅ shipped (v0.8 exact-match; v0.13 semantic)

Exact-match response caching: identical requests (retries, idempotent agent
re-sends) skip the upstream call entirely — zero cost, zero latency. Bounded LRU
with TTL (`CACHE_TTL`/`CACHE_MAX`), in-memory only (never persisted — responses
can echo request PII), non-streaming only (like retry/fallback).
`bifrost_cache_hits_total` + `bifrost_cache_misses_total`, plus a hit-rate on the
dashboard. Semantic caching (v0.13) extends this to *similar* requests via
embeddings, tracked as `bifrost_semantic_cache_hits_total`/`..._misses_total`.

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

### 8. Model catalog with metadata — ✅ shipped (v0.15)

`GET /v1/models` (and `/v1/models/{name}`) returns each client model with its
privacy tier (`private`/`local`/`cloud`), backend, upstream model, capabilities,
and per-1M-token pricing — so an agent can ask "what's available and what's
right for me" instead of hardcoding a model. `/api/tags` sets `details.family`
to the same privacy tier for Ollama clients.

### 9. Retrieval endpoint — ❌ dropped

Was a stretch goal: embeddings plus vector search over the vault, so agents got
semantic search without each reimplementing it. Dropped rather than deferred. It
needs a real vector store and a corpus-indexing lifecycle, and neither belongs in
a single binary that holds no external state. Point agents at a real store.

### 10. Key pool — ✅ shipped (v0.16)

A backend can hold many keys (different billing plans, rate-limit buckets).
Bifrost rotates across them (weighted round-robin) and, on a `429`/`401`/`403`,
cools the offending key and transparently retries the next one — before backend
failover applies. Keys live in a SOPS-encrypted Secret (`KEYPOOL`), never in a
ConfigMap or the repo. Per-key cost/plan accounting is a follow-up.

### 11. Tenant keys and attribution — ✅ shipped (v0.18)

Keys are minted by Bifrost, not written by hand: `POST /admin/keys` returns the
secret once and stores only its SHA-256 in `KEYS_FILE`. Each key is a tenant —
its own monthly budget and rate limit, its own `last_used`, its own `spend_month`
in `GET /admin/keys`, revocable with `DELETE /admin/keys/{id}` without a restart
(revocation is soft, so ledger history still resolves).

**Decision:** a key is the tenant, and its name is the `app` label used
everywhere else. That keeps budgets, metrics and the dashboard working unchanged
while making the identity unspoofable, instead of inventing a second attribution
axis beside `app`.

**Decision:** anonymous callers are a policy, not an accident. `ANON=open`
preserves the keyless behaviour, `limited` admits them but applies
`ANON_RATE_LIMIT` *per source IP* (one abuser must not starve every other
anonymous client) and `ANON_BUDGET` as a shared monthly cap, and `closed`
refuses. Turning on `KEYS_FILE` or `APP_KEYS` defaults to `closed`; a presented
but invalid key is always `401` and never falls through to anonymous.

### 12. Portability — ✅ shipped (v0.19)

Same binary, no cluster. The portability work was mostly making failure modes
honest rather than adding features:

- **State is checked at startup.** `LEDGER_FILE` and `KEYS_FILE` had no parent
  directory creation, and a failed append was one log line per request — spend
  silently stopped being recorded and budgets read zero. `checkState` now
  creates the directory, proves it writable with a temp file, and refuses to
  start otherwise. An unreadable ledger is no longer conflated with "no ledger
  yet".
- **`/data` is part of the image**, owned by 65534. Docker creates a named
  volume root-owned when the mount point is absent, which made the first key
  mint fail with EACCES.
- **SIGTERM drains** for up to 10s instead of cutting a half-written stream.
- **`-health`** lets a scratch image answer `HEALTHCHECK` without curl.
- **`scripts/portability-test.sh`** is the acceptance test, run in CI on every
  PR: fresh volume, unprivileged, mint → chat → restart → still valid, SIGTERM
  exits 0, unwritable state fails loudly, healthcheck present. The published
  manifest is checked for both `linux/amd64` and `linux/arm64`.

Known limit, documented rather than solved: the ledger and key registry are
single-writer files. Two replicas on one volume will interleave appends and
double-count budgets.

## Non-goals

- **A model runner** — vLLM/llama.cpp already own that.
- **A training pipeline.**
- **A full vector database, or a retrieval endpoint that implies one** — delegate
  to an existing store rather than embed one. (See initiative 9.)
- **An admin UI as a product** — the dashboard is for reading, not for operating
  a fleet of tenants. Per-tenant key management, when it lands, will be an API,
  not a console.

Bifrost stays thin: its value is the *decision layer*, not the compute.
