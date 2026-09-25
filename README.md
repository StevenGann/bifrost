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

## Quick start

```bash
docker run -d --name bifrost -p 11434:11434 \
  -e UPSTREAM_API_KEY=sk-... \
  ghcr.io/stevengann/bifrost:latest
```

Then point any Ollama client at `http://<host>:11434`.

## Configuration

| Env | Default | Description |
|-----|---------|-------------|
| `PORT` | `11434` | Listen port |
| `UPSTREAM_BASE_URL` | `https://api.deepseek.com` | OpenAI-compatible backend base URL |
| `UPSTREAM_API_KEY` | *(none)* | Bearer key for the upstream |
| `MODELS` | `deepseek-v4-flash` | Comma-separated model list; `alias=upstream` to rename |
| `PRICING` | *(built-in DeepSeek rates)* | JSON `{"model":{"input":x,"output":y}}` in USD per 1M tokens, merged over the DeepSeek defaults |

Example with aliases:

```bash
MODELS="deepseek-v4-flash,deepseek-v4-pro,coach=deepseek-v4-flash"
```

Clients then see three models; `coach` routes to `deepseek-v4-flash` upstream.

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
