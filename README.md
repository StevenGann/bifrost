# Bifrost

An **Ollama-compatible LLM gateway** — the homelab's LLM control plane. It speaks
the native Ollama API (`/api/tags`, `/api/chat`, `/api/generate`) *and* the
OpenAI-compatible `/v1` API on port `11434`, and proxies every request to any
OpenAI-compatible backend — DeepSeek, vLLM, and so on.

Named for the Norse rainbow bridge: it carries requests from Ollama-speaking
clients (Midgard) to the LLM backend (Asgard).

See **[docs/ROADMAP.md](docs/ROADMAP.md)** for where this is going (multi-backend
routing, embeddings, privacy routing, caching, resilience, and more).

## Why

- **Drop-in Ollama replacement.** Point any tool that wants an Ollama URL at
  Bifrost and it just works — no local GPU, no model downloads, no disk.
- **Backend-swappable.** Repoint `UPSTREAM_BASE_URL` to switch from DeepSeek to a
  local vLLM/Ollama server without touching any client.
- **Observable.** Prometheus `/metrics` with token, cost, and latency accounting.
- **Tiny.** A single ~10 MB static Go binary, no runtime deps, no state.
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

Non-streaming completions retry transient failures (network, `429`, `5xx`) with
exponential backoff (`RETRIES`) and fall back through a model chain (`FALLBACKS`)
when the primary fails for good. Streaming and `/v1` passthrough stay
single-attempt — retrying a half-flushed stream would corrupt the client.

## API surface

- `GET /api/tags` — model list (Ollama format)
- `POST /api/chat` — chat, streaming or not (Ollama format)
- `POST /api/generate` — completion (Ollama format)
- `GET /api/version` — `0.1.0-bifrost`
- `GET /v1/models` — model list (OpenAI format)
- `POST /v1/chat/completions` — passthrough (OpenAI format)
- `GET /healthz` — liveness
- `GET /metrics` — Prometheus metrics

Embeddings (`/api/embeddings`, `/api/embed`) return `501` — most OpenAI-compatible
upstreams (including DeepSeek) don't expose them.

## License

MIT. See [LICENSE](LICENSE).
