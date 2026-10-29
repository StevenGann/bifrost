# Deploying Bifrost

Bifrost is one static binary with no runtime dependencies. It runs anywhere a
container or a Linux binary runs — this page is the version that assumes nothing
about our cluster.

## What it needs

- A writable directory for state (or no state at all, if you don't want budgets
  and keys to persist).
- Network access to every backend in `BACKENDS`.
- Nothing else. No database, no queue, no sidecar.

## Quick start with compose

```bash
git clone https://github.com/StevenGann/bifrost.git
cd bifrost/deploy/compose
cp .env.example .env
$EDITOR .env                 # set ADMIN_KEY, point BACKENDS at your model
docker compose up -d
curl -s localhost:11434/v1/models | jq
```

Mint a key for a client:

```bash
curl -s -X POST localhost:11434/admin/keys \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"my-app","budget":20,"rate_limit":120}'
```

The response contains `key` once. It is not recoverable — Bifrost stores only
its SHA-256. Put it in the client's config and move on.

## Without compose

```bash
docker run -d --name bifrost -p 11434:11434 \
  -e BACKENDS='[{"name":"ollama","base_url":"http://host.docker.internal:11434","local":true}]' \
  -e ROUTES='{"coach":"ollama/llama3.2"}' \
  -e LEDGER_FILE=/data/spend.jsonl -e KEYS_FILE=/data/keys.json \
  -e ADMIN_KEY="$(openssl rand -hex 32)" \
  -v bifrost-data:/data \
  ghcr.io/stevengann/bifrost:latest
```

Or run the binary directly: `BACKENDS=... ROUTES=... ./bifrost`.

## State

Two files, both optional, both on the volume:

| Path | What it is | If you lose it |
|------|-----------|----------------|
| `LEDGER_FILE` | append-only JSONL, one line per request: tenant, key id, model, cost, tokens/chars | budgets restart from zero for the current month |
| `KEYS_FILE` | tenant registry: id, name, SHA-256, created, last used, revoked | every minted key stops working |

`docker compose down -v` deletes both. Back up the volume, not the container.

The ledger is month-scoped: on the first request of a new UTC month Bifrost
starts a fresh counter, and entries for earlier months stay in the file as
history. Pruning it is safe but loses history, not state.

### One instance only

The ledger and key registry are plain files with a single writer. Two replicas
sharing a volume will interleave appends and double-count budgets. Run Bifrost
as a single instance; if you need HA, put a load balancer in front of one pod
and fail over at that layer, or point both replicas at different volumes and
accept that budgets are per-replica.

## Secrets

Backend credentials are read from environment variables named by
`api_key_env`, and never from the image. Use `env_file`, your orchestrator's
secret store, or `docker run --env-file`. `ADMIN_KEY` should be long and random:

```bash
openssl rand -hex 32
```

If `ADMIN_KEY` is empty the admin endpoints are not registered at all — that is
the correct default for a deployment where nobody mints keys.

## Identity and anonymous access

Every request is attributed to a tenant. With `KEYS_FILE` set, a client presents
`Authorization: Bearer bk_<id>_<secret>` and its tenant name appears in metrics,
the dashboard, budgets and the ledger.

`ANON` decides what happens without a token:

- `open` — admitted as `anonymous`. The default when no keys exist.
- `limited` — admitted, with `ANON_RATE_LIMIT` per source IP and `ANON_BUDGET`
  as a shared monthly ceiling.
- `closed` — `401`. The default once `KEYS_FILE` or `APP_KEYS` is set.

A token that is present but wrong is always `401`; it never falls back to
anonymous.

`ANON_RATE_LIMIT` buckets on the connection's source address. Behind a reverse
proxy every client shares the proxy's address, so either terminate per-client
connections or keep the limiter off and rely on keys.

## Operating it

- **Health:** `GET /healthz`, or `/bifrost -health` (the binary probes its own
  port, which is what the image's `HEALTHCHECK` uses).
- **Shutdown:** `SIGTERM` drains in-flight requests for up to 10s. Give the
  container at least that long (`stop_grace_period: 15s`).
- **Metrics:** `GET /metrics` (Prometheus) and `GET /dashboard`.
- **Startup fails loudly** if `LEDGER_FILE` or `KEYS_FILE` points somewhere
  unwritable. That is deliberate: the alternative is a gateway that looks
  healthy while silently not recording spend.

## Behind a reverse proxy

Bifrost speaks plain HTTP. Terminate TLS at the proxy and forward to `:11434`.
Keep `/admin/keys` off the public listener if the proxy is internet-facing —
it is protected by `ADMIN_KEY`, but there is no reason to publish it. The
dashboard and `/metrics` expose tenant names and spend, so treat them as
internal too.

## Migrating from Kubernetes

The image and the environment variables are the same. Copy `/data/spend.jsonl`
and `/data/keys.json` out of the old PVC into the new volume, keep `ADMIN_KEY`
unchanged so existing keys keep working, and expect no changes to clients.
