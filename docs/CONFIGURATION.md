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
| `APP_KEYS` | JSON `{"app":"bearer-key"}` — imported as **static** tenant keys; when set, requires a valid key (else `401`) |
| `KEYS_FILE` | Path to the tenant key registry (e.g. `/data/keys.json`). Bifrost mints keys here; only SHA-256 hashes are stored |
| `ADMIN_KEY` | Bearer token that authorises `/admin/keys`. Unset = the admin API does not exist |
| `ANON` | `open` (default with no keys), `limited` (anonymous allowed, subject to `ANON_*`), or `closed` (default once `KEYS_FILE`/`APP_KEYS` is set) |
| `ANON_RATE_LIMIT` | Requests/minute per source IP for anonymous callers (default `0` = unlimited) |
| `ANON_BUDGET` | Monthly USD cap across all anonymous callers (default `0` = unlimited) |
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
| `AUDIO_ROUTES` | JSON `{"tts":"backend/voice-model"}` — speech routes, kept out of `ROUTES` so they are not advertised as chat models |
| `TTS_VOICES` | JSON `{"alloy":"<backend voice id>"}` — maps a caller-facing voice to the backend's voice |
| `TTS_VOICE` | Default backend voice id when the request names none (or names an unmapped short name) |
| `TTS_COST_PER_CHAR` | USD per character synthesized (default `0`) — recorded in the ledger with the character count |

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

## Tenant keys

Bifrost can mint its own client keys and use them as the unit of attribution and
policy. Set `KEYS_FILE` to a writable path and `ADMIN_KEY` to a secret you keep
out of the config:

```bash
KEYS_FILE=/data/keys.json
ADMIN_KEY=<long random string, kept in a secret>
ANON=closed          # optional: refuse callers with no key
```

Mint one per client:

```bash
curl -sS -X POST http://gateway:11434/admin/keys \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"hermes","budget":5,"rate_limit":120}'
```

```json
{"id":"a1b2c3d4","name":"hermes","key":"bk_a1b2c3d4_…","created":"…","notice":"this is the only time the key is shown"}
```

- The key is returned **once**. Only its SHA-256 is stored, so the keys file
  cannot be used to authenticate.
- `GET /admin/keys` lists keys with `created`, `last_used`, `revoked`,
  `spend_month` and `budget` — never secret material.
- `DELETE /admin/keys/{id}` revokes. The record is kept so ledger history still
  resolves to the right tenant.
- Per-key `budget` (USD/month) and `rate_limit` (req/min) override the global
  defaults for that tenant. `BUDGET` remains a ceiling over everything.
- Every ledger line carries `app` (tenant name) and `key` (key id).
- The admin API is **not registered** unless `ADMIN_KEY` is set.

### Anonymous callers

`ANON` decides what happens with no token:

- `open` — admitted, attributed to `anonymous`. The default when no keys are
  configured, so a keyless deployment behaves as it always did.
- `limited` — admitted, but `ANON_RATE_LIMIT` applies **per source IP** and
  `ANON_BUDGET` caps anonymous spend per month across all callers.
- `closed` — `401`. The default once `KEYS_FILE` or `APP_KEYS` is set.

A token that is *presented but invalid* is always a `401`. A wrong key never
degrades silently into anonymous access.

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
- `POST /v1/audio/speech` — text-to-speech (OpenAI format), translated per backend `kind`
- `GET /healthz` — liveness
- `GET /metrics` — Prometheus metrics
- `GET /dashboard` — live HTML dashboard; `GET /dashboard.json` — the same snapshot as JSON, polled by the page
- `POST /admin/keys` — mint a tenant key (returns the secret once); `GET /admin/keys` — list tenants with spend and last use; `DELETE /admin/keys/{id}` — revoke. Only registered when `ADMIN_KEY` is set, and each requires `Authorization: Bearer $ADMIN_KEY`

## Speech

`POST /v1/audio/speech` accepts the OpenAI body — `{model, input, voice,
response_format}` — and speaks whatever backend the route points at. The
backend's `kind` selects the dialect:

| `kind` | Upstream shape | Auth |
|--------|----------------|------|
| `openai` (default) | `POST /v1/audio/speech` | `Authorization: Bearer` |
| `elevenlabs` | `POST /v1/text-to-speech/{voice_id}?output_format=…` | `xi-api-key` |

```bash
BACKENDS='[{"name":"elevenlabs","base_url":"https://api.elevenlabs.io","kind":"elevenlabs","api_key_env":"ELEVENLABS_API_KEY"}]'
AUDIO_ROUTES='{"tts":"elevenlabs/eleven_multilingual_v2"}'
TTS_VOICES='{"alloy":"pNInz6obpgDQGcFmaJgB"}'
TTS_COST_PER_CHAR='0.00018'
FALLBACKS='{"tts":"tts-local"}'
```

A failing voice backend falls over to the route named in `FALLBACKS`, the same
way chat does — the point of routing speech through the gateway. Supported
`response_format` per kind: `elevenlabs` takes `mp3`, `opus`, `pcm`; `openai`
takes those plus `aac`, `flac`, `wav`. An unsupported format is a `400` rather
than a silent substitution.

Speech has no tokens, so the ledger books characters (`"chars"`) alongside cost
and the dashboard carries a `chars` column.
