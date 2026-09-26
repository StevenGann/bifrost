# Bifrost

An **Ollama-compatible LLM gateway** — the homelab's LLM control plane. It speaks
the native Ollama API (`/api/tags`, `/api/chat`, `/api/generate`) *and* the
OpenAI-compatible `/v1` API on port `11434`, and proxies every request to any
OpenAI-compatible backend — DeepSeek, vLLM, and so on.

Named for the Norse rainbow bridge: it carries requests from Ollama-speaking
clients (Midgard) to the LLM backend (Asgard).

See **[docs/ROADMAP.md](docs/ROADMAP.md)** for where this is going (multi-backend
routing, embeddings, privacy routing, caching, resilience, governance, and more).

## Why

- **Drop-in Ollama replacement.** Point any tool that wants an Ollama URL at
  Bifrost and it just works — no local GPU, no model downloads, no disk.
- **Backend-swappable.** Repoint `UPSTREAM_BASE_URL` to switch from DeepSeek to a
  local vLLM/Ollama server without touching any client.
- **Observable.** Prometheus `/metrics` with token, cost, and latency accounting.
- **Tiny.** A single ~10 MB static Go binary, no runtime deps, near-zero state
  (one optional spend ledger file).
- **Private by default.** PII is scrubbed from cloud-bound traffic, and
  `private:*` models never leave the LAN.

## Quick start

```bash
docker run -d --name bifrost -p 11434:11434 \
  -e UPSTREAM_API_KEY=sk-... \
  ghcr.io/stevengann/bifrost:latest
```

Then point any Ollama client at `http://<host>:11434`.

## Configuration

Bifrost routes client-facing model names to one or more upstream backends.

### Multi-backend (recommended)

| Env | Description |
|-----|-------------|
| `BACKENDS` | JSON array of backends: `[{"name":"deepseek","base_url":"https://api.deepseek.com","api_key_env":"UPSTREAM_API_KEY"}]` |
| `ROUTES` | JSON object mapping client model → `backend/upstream-model` |
| `PORT` | Listen port (default `11434`) |
| `RETRIES` | Retries for transient failures — network, `429`, `5xx` (default `2`) |
| `FALLBACKS` | JSON `{"model":"fallback-model"}` — fall back when the primary fails |
| `PRICING` | JSON `{"model":{"input":x,"output":y}}` USD/1M tokens, merged over DeepSeek defaults |
| `BUDGET` | Monthly spend cap in USD (default `0` = unlimited) — over budget → `402` |
| `APP_BUDGETS` | JSON `{"app":usd}` per-app monthly cap |
| `RATE_LIMIT` | Per-app requests/minute (default `0` = unlimited) — exceeded → `429` |
| `APP_KEYS` | JSON `{"app":"bearer-key"}` — when set, requires a valid key (else `401`) |
| `LEDGER_FILE` | Path to the JSONL spend ledger (e.g. `/data/spend.jsonl`) |
| `CACHE_TTL` | Exact-match cache TTL in seconds (default `300`; `0` disables) |
| `CACHE_MAX` | Max cached entries (default `256`) |
| `SEMANTIC_CACHE` | Enable semantic (near-neighbor) caching — embeds prompts and serves similar cached responses (default `false`) |
| `SEMANTIC_THRESHOLD` | Cosine-similarity threshold for a semantic hit (default `0.92`) |
| `EMBED_MODEL` | Client model used to embed prompts for semantic caching (default `embed`) |
| `CIRCUIT_THRESHOLD` | Consecutive failures before a backend's circuit opens (default `3`) |
| `CIRCUIT_COOLDOWN` | Seconds before a half-open probe is attempted (default `30`) |
| `HEALTH_INTERVAL` | Seconds between backend health polls (default `15`; `0` disables) |
| `INDEX_FILE` | Path to persist the retrieval index as JSON (default `` — in-memory only) |
| `CHUNK_SIZE` | Max chars per ingested document chunk (default `1000`) |
| `CHUNK_OVERLAP` | Chunk overlap in chars (default `100`) |
| `RETRIEVE_K` | Top-k chunks retrieved for RAG (default `3`) |

```bash
BACKENDS='[{"name":"deepseek","base_url":"https://api.deepseek.com","api_key_env":"UPSTREAM_API_KEY"}]'
ROUTES='{"coach":"deepseek/deepseek-v4-flash","deepseek-v4-flash":"deepseek/deepseek-v4-flash","deepseek-v4-pro":"deepseek/deepseek-v4-pro"}'
```

`api_key_env` names an env var holding the backend's bearer key (keeps secrets
out of the config). The **first** backend in `BACKENDS` is the default — unknown
model names route to it unchanged.

### Single upstream (legacy)

If `BACKENDS` is unset, Bifrost falls back to the original single-upstream config:

| Env | Description |
|-----|-------------|
| `UPSTREAM_BASE_URL` | Backend base URL (default `https://api.deepseek.com`) |
| `UPSTREAM_API_KEY` | Bearer key |
| `MODELS` | Comma-separated `alias=upstream` list |

## Metrics

`GET /metrics` exposes Prometheus text-format metrics for completion traffic:

- `bifrost_requests_total` — requests by model, app, endpoint, status.
- `bifrost_tokens_total` — input/output tokens by model, app.
- `bifrost_cost_usd_total` — estimated cost (cache-miss rates, peak/off-peak aware).
- `bifrost_errors_total` — upstream errors by model, app, endpoint.
- `bifrost_request_duration_seconds` — latency histogram by model, app.

Token counts are **accurate, not estimated**: Bifrost injects
`stream_options.include_usage` into streaming requests and reads the usage block
from the final chunk.

To split metrics by caller, send an `X-Bifrost-App` header (e.g.
`X-Bifrost-App: patzer`); otherwise requests are labeled `app="unknown"`.

Cost is priced at DeepSeek's official cache-miss rates, doubled during peak hours
(Mon–Fri 01:00–04:00 and 06:00–10:00 UTC). Override or add models via `PRICING`.

## Privacy

- **PII redaction.** Every request bound for a *cloud* backend (base URL not
  loopback/RFC1918/`.lab`) has obvious identifiers scrubbed before it leaves the
  LAN: emails, phones, card numbers, SSNs, IPs, API/SSH keys, PEM private keys.
- **`private:` models.** A client model named `private:*` refuses to route to a
  non-local backend — it returns `400` rather than silently sending data to the
  cloud. Point `private:*` at a local backend in `ROUTES` and it just works.
- **Local auto-detection.** Loopback, RFC1918, and `.lab`/`.local`/etc. backends
  are treated as local (never redacted); override with `"local": true`/`false`.

## Resilience

Failures are absorbed transparently: the client sees one slow success, never an
internal backend failure. Every path — chat (streaming or not), the `/v1`
pass-through, and embeddings — retries transient failures (network, `429`,
`5xx`) with exponential backoff (`RETRIES`) and fails over through a model chain
(`FALLBACKS`). Streaming retries/fails over up to the first byte flushed; once a
stream starts it's committed and can't be replayed, so mid-stream failures are
the only thing a client can observe.

Each backend also gets a circuit breaker: after `CIRCUIT_THRESHOLD` consecutive
failures it opens, so requests fail fast (503) — or fail over instantly to a
fallback — instead of hanging on a downed backend. This matters for itinerant
local workers (e.g. Epsilon) that come and go. A health poller probes every
backend on `HEALTH_INTERVAL` and pre-opens the circuit while a backend is
unreachable, so absence is *noticed* before any request discovers it. After
`CIRCUIT_COOLDOWN` (or a poll confirming recovery) it half-opens to admit a
probe and re-close on success. Local backends use a short 3s dial timeout so a
powered-off worker fails over in seconds, not 30s. Surfaced as
`bifrost_circuit_trips_total`, `bifrost_circuit_open`, and
`bifrost_backend_healthy`.

## Governance

Hard monthly budgets (global `BUDGET` + per-app `APP_BUDGETS`), per-app rate
limits (`RATE_LIMIT`), and optional per-app bearer auth (`APP_KEYS`). Spend is
tracked in a durable JSONL ledger (`LEDGER_FILE`) so budgets survive restarts.
Over budget → `402`, rate-limited → `429`, unauthenticated → `401`.

Without `APP_KEYS`, the `X-Bifrost-App` header still identifies callers for
metrics — but it is self-reported (any client can claim to be any app). Deploy
`APP_KEYS` when per-app budgets need to be authoritative.

## Caching

Identical non-streaming requests (retries, idempotent agent re-sends) are served
from an in-memory exact-match cache, skipping the upstream call entirely — zero
cost, zero latency. Bounded LRU with TTL (`CACHE_TTL`/`CACHE_MAX`). Never
persisted (cached responses can echo request PII). `bifrost_cache_hits_total` /
`bifrost_cache_misses_total` in `/metrics`, hit-rate on the dashboard.

Opt-in **semantic caching** (`SEMANTIC_CACHE=true`) extends this to *similar*
requests: the prompt is embedded via the `EMBED_MODEL` route and a near-neighbor
cached response is served when its cosine similarity clears `SEMANTIC_THRESHOLD`.
This turns the local embedding brain into real cost savings — a reworded question
hits the cache instead of the upstream. It degrades gracefully: if the embedding
backend is down, requests simply skip semantic caching and proceed. Tracked as
`bifrost_semantic_cache_hits_total` / `bifrost_semantic_cache_misses_total`.

## Embeddings

`/api/embeddings` (+ `/api/embed`) and `/v1/embeddings` route to whichever backend
holds an embedding model — add a `ROUTES` entry (e.g.
`embed → epsilon/nomic-embed-text`) pointing at a local model. Bifrost translates
between the Ollama-native and OpenAI embedding shapes.

## Retrieval (RAG)

Bifrost doubles as a small private retrieval engine. Ingest documents (chunked
then embedded via `EMBED_MODEL`), then let `private:*` models answer from them:

- `POST /api/documents` — ingest `{"documents":[{"name","text"},…]}`; each is
  chunked (`CHUNK_SIZE`/`CHUNK_OVERLAP`), embedded, and stored in an in-memory
  vector index (linear-scan top-k — sized for a homelab, no ANN needed).
- `POST /api/retrieve` — `{"query","k"}` returns top-k chunks with cosine scores.

When a request targets a `private:*` model **and** the index is non-empty,
Bifrost automatically retrieves the top-`RETRIEVE_K` chunks for the query and
injects them as a system message, grounding the answer in your documents. It is
a silent no-op while the index is empty or the embedding backend is unreachable,
so a cold or degraded Bifrost behaves exactly like before. The index is persisted
to `INDEX_FILE` when set (opt-in; empty means in-memory only). Exposed as
`bifrost_index_chunks`.

## API surface

- `GET /api/tags` — model list (Ollama format)
- `POST /api/chat` — chat, streaming or not (Ollama format)
- `POST /api/generate` — completion (Ollama format)
- `POST /api/embeddings` / `POST /api/embed` — embeddings (Ollama format)
- `POST /api/documents` — ingest documents into the retrieval index
- `POST /api/retrieve` — retrieve top-k chunks for a query
- `GET /api/version` — `0.1.0-bifrost`
- `GET /v1/models` — model list (OpenAI format)
- `POST /v1/chat/completions` — passthrough (OpenAI format)
- `POST /v1/embeddings` — embeddings (OpenAI format)
- `GET /healthz` — liveness
- `GET /metrics` — Prometheus metrics

## License

MIT. See [LICENSE](LICENSE).
