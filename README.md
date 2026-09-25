# Bifrost

An **Ollama-compatible LLM gateway**. It speaks the native Ollama API (`/api/tags`, `/api/chat`, `/api/generate`) *and* the OpenAI-compatible `/v1` API on port `11434`, and proxies every request to any OpenAI-compatible backend — DeepSeek, vLLM, and so on.

Named for the Norse rainbow bridge: it carries requests from Ollama-speaking clients (Midgard) to the LLM backend (Asgard).

## Why

- **Drop-in Ollama replacement.** Point any tool that wants an Ollama URL at Bifrost and it just works — no local GPU, no model downloads, no disk.
- **Backend-swappable.** Repoint `UPSTREAM_BASE_URL` to switch from DeepSeek to a local vLLM/Ollama server without touching any client.
- **Tiny.** A single ~10 MB static Go binary, no runtime deps, no state.

## Quick start

```bash
docker run -d --name bifrost -p 11434:11434 \
  -e UPSTREAM_API_KEY=sk-... \
  ghcr.io/StevenGann/bifrost:latest
```

Then point any Ollama client at `http://<host>:11434`.

## Configuration

| Env | Default | Description |
|-----|---------|-------------|
| `PORT` | `11434` | Listen port |
| `UPSTREAM_BASE_URL` | `https://api.deepseek.com` | OpenAI-compatible backend base URL |
| `UPSTREAM_API_KEY` | *(none)* | Bearer key for the upstream |
| `MODELS` | `deepseek-v4-flash` | Comma-separated model list; `alias=upstream` to rename |

Example with aliases:

```bash
MODELS="deepseek-v4-flash,deepseek-v4-pro,coach=deepseek-v4-flash"
```

Clients then see three models; `coach` routes to `deepseek-v4-flash` upstream.

## API surface

- `GET /api/tags` — model list (Ollama format)
- `POST /api/chat` — chat, streaming or not (Ollama format)
- `POST /api/generate` — completion (Ollama format)
- `GET /api/version` — `0.1.0-bifrost`
- `GET /v1/models` — model list (OpenAI format)
- `POST /v1/chat/completions` — passthrough (OpenAI format)
- `GET /healthz` — liveness

Embeddings (`/api/embeddings`, `/api/embed`) return `501` — most OpenAI-compatible upstreams (including DeepSeek) don't expose them.

## License

MIT. See [LICENSE](LICENSE).
