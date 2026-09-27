# Configuration

Bifrost is configured entirely through environment variables. There is no config
file. Everything below can be passed to the container with `-e`, or loaded from
a Kubernetes Secret/ConfigMap.

## Secrets

API keys never go in config. A backend's `api_key_env` names an environment
variable that holds the bearer key — keep that variable in a Secret (SOPS, k8s
Secret, Docker secret), never in a ConfigMap or the repo.

## Multi-backend (recommended)

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
| `KEYPOOL` | JSON map of backend → key list for multi-key rotation (default empty — single key via `api_key_env`) |

```bash
BACKENDS='[{"name":"deepseek","base_url":"https://api.deepseek.com","api_key_env":"UPSTREAM_API_KEY"}]'
ROUTES='{"coach":"deepseek/deepseek-v4-flash","deepseek-v4-flash":"deepseek/deepseek-v4-flash","deepseek-v4-pro":"deepseek/deepseek-v4-pro"}'
```

`api_key_env` names an env var holding the backend's bearer key (keeps secrets
out of the config). The **first** backend in `BACKENDS` is the default — unknown
model names route to it unchanged.

## Key pool

A backend can hold **many** keys instead of one. Point `KEYPOOL` at a JSON map
of backend name → key list:

```json
{"deepseek": [
  {"id": "ds-offpeak", "key": "sk-…", "plan": "off-peak", "weight": 1},
  {"id": "ds-work",    "key": "sk-…", "plan": "work",    "weight": 2}
]}
```

- **Weighted round-robin** — `weight` (default `1`) spends one key more than
  another.
- **Transparent rotation** — a rate-limited/rejected key (`429`/`401`/`403`) is
  cooled ~30s while the retry moves to the next key, before backend failover.
- `id`/`plan` are labels for observability; tracked as
  `bifrost_key_rotations_total{backend}`.
- No `KEYPOOL` → the single `api_key_env` key is used unchanged.

## Single upstream (legacy)

If `BACKENDS` is unset, Bifrost falls back to the original single-upstream
config:

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
- `bifrost_key_rotations_total` — key rotations by backend.
- `bifrost_cache_hits_total` / `bifrost_cache_misses_total` — exact-match cache.
- `bifrost_semantic_cache_hits_total` / `bifrost_semantic_cache_misses_total` — semantic cache.
- `bifrost_circuit_trips_total` / `bifrost_circuit_open` / `bifrost_backend_healthy` — resilience.

Token counts are **accurate, not estimated**: Bifrost injects
`stream_options.include_usage` into streaming requests and reads the usage block
from the final chunk.

To split metrics by caller, send an `X-Bifrost-App` header (e.g.
`X-Bifrost-App: patzer`); otherwise requests are labeled `app="unknown"`.

Cost is priced at DeepSeek's official cache-miss rates, doubled during peak
hours (Mon–Fri 01:00–04:00 and 06:00–10:00 UTC). Override or add models via
`PRICING`.

## API surface

- `GET /api/tags` — model list (Ollama format)
- `POST /api/chat` — chat, streaming or not (Ollama format)
- `POST /api/generate` — completion (Ollama format)
- `POST /api/embeddings` / `POST /api/embed` — embeddings (Ollama format)
- `GET /api/version` — `0.1.0-bifrost`
- `GET /v1/models` — model list with privacy tier, backend, upstream, and pricing (OpenAI format)
- `GET /v1/models/{name}` — single model's metadata
- `POST /v1/chat/completions` — passthrough (OpenAI format)
- `POST /v1/embeddings` — embeddings (OpenAI format)
- `GET /healthz` — liveness
- `GET /metrics` — Prometheus metrics
