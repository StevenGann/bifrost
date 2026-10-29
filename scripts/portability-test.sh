#!/usr/bin/env bash
# Portability test: prove the published image runs outside our cluster.
#
#   BIFROST_IMAGE=ghcr.io/stevengann/bifrost:latest scripts/portability-test.sh
#
# Needs a working docker daemon and docker compose v2. Exits non-zero if any
# expectation fails. Everything it creates is removed on exit.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
COMPOSE_FILE="$REPO_ROOT/deploy/compose/compose.test.yml"
PROJECT=bifrost-portability
IMAGE=${BIFROST_IMAGE:?set BIFROST_IMAGE to the image under test}
BASE=http://127.0.0.1:11434
ADMIN=test-admin-key
OUT=$(mktemp)
trap 'rm -f "$OUT"; docker compose -p "$PROJECT" -f "$COMPOSE_FILE" down -v --remove-orphans >/dev/null 2>&1 || true' EXIT

pass=0
fail=0

check() { # check <name> <0|1> [detail]
  if [ "$2" = "0" ]; then
    printf 'PASS %s\n' "$1"
    pass=$((pass + 1))
  else
    printf 'FAIL %s  %s\n' "$1" "${3:-}"
    fail=$((fail + 1))
  fi
}

dc() { docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"; }

wait_healthy() {
  for _ in $(seq 1 60); do
    curl -fsS "$BASE/healthz" >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

# status <method> <path> [bearer] [json-body] -> prints the HTTP code, body in $OUT
status() {
  local method=$1 path=$2 bearer=${3:-} body=${4:-}
  local args=(-s -o "$OUT" -w '%{http_code}' -X "$method" "$BASE$path")
  [ -n "$bearer" ] && args+=(-H "Authorization: Bearer $bearer")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  curl "${args[@]}"
}

json_field() { python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$OUT" "$1"; }
spend_total() {
  status GET /admin/keys "$ADMIN" >/dev/null
  python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(round(sum(k["spend_month"] for k in d["keys"]),9))' "$OUT"
}

echo "== image: $IMAGE =="

# 1. Cold start on an empty volume, running as the image's unprivileged user.
export BIFROST_IMAGE="$IMAGE"
if ! dc up -d >/dev/null 2>&1 || ! wait_healthy; then
  echo "FAIL the container did not become healthy"
  dc logs --tail=40 bifrost || true
  exit 1
fi
check "cold start on a fresh volume" 0

# 2. Model catalog comes from the route table.
code=$(status GET /v1/models)
[ "$code" = "200" ] && grep -q deepseek-v4-flash "$OUT" \
  && check "GET /v1/models lists the route" 0 || check "GET /v1/models lists the route" 1 "$code"

# 3. Anonymous is refused when ANON=closed.
code=$(status POST /v1/chat/completions "" '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}')
[ "$code" = "401" ] && check "anonymous refused (ANON=closed)" 0 || check "anonymous refused (ANON=closed)" 1 "$code"

# 4. The admin API is gated.
code=$(status POST /admin/keys wrong-admin '{"name":"nope"}')
[ "$code" = "401" ] && check "admin rejects a wrong ADMIN_KEY" 0 || check "admin rejects a wrong ADMIN_KEY" 1 "$code"

# 5. Mint a key on the fresh volume — the call that fails when /data is owned
#    by root instead of the runtime user.
code=$(status POST /admin/keys "$ADMIN" '{"name":"hermes","budget":5}')
KEY=""
[ "$code" = "201" ] && KEY=$(json_field key 2>/dev/null || true)
[ -n "$KEY" ] && check "mint a key on a fresh volume" 0 || check "mint a key on a fresh volume" 1 "$code $(cat "$OUT")"

# 6. That key works and reaches the upstream stub.
code=$(status POST /v1/chat/completions "$KEY" '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}')
[ "$code" = "200" ] && grep -q 'stub ok' "$OUT" \
  && check "keyed chat completes through the stub" 0 || check "keyed chat completes through the stub" 1 "$code"

# 7. Spend was booked to the ledger.
BEFORE=$(spend_total)
python3 -c 'import sys;sys.exit(0 if float(sys.argv[1])>0 else 1)' "$BEFORE" \
  && check "spend booked to the ledger ($BEFORE USD)" 0 || check "spend booked to the ledger" 1 "total=$BEFORE"

# 8. Restart: the key and the spend must both survive. This is the guarantee
#    that only holds if /data is a volume and not the container filesystem.
dc restart bifrost >/dev/null 2>&1
if ! wait_healthy; then echo "FAIL restart never became healthy"; dc logs --tail=40 bifrost || true; exit 1; fi
AFTER=$(spend_total)
[ "$AFTER" = "$BEFORE" ] && check "spend survives a restart ($AFTER)" 0 || check "spend survives a restart" 1 "$BEFORE -> $AFTER"
code=$(status POST /v1/chat/completions "$KEY" '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"again"}]}')
[ "$code" = "200" ] && check "key survives a restart" 0 || check "key survives a restart" 1 "$code"

# 9. SIGTERM drains instead of being killed. Docker would kill after 10s, so
#    exit code 0 means the server shut itself down.
dc stop -t 10 bifrost >/dev/null 2>&1
exitcode=$(docker inspect -f '{{.State.ExitCode}}' "$(dc ps -aq bifrost)")
[ "$exitcode" = "0" ] && check "SIGTERM exits cleanly (code 0)" 0 || check "SIGTERM exits cleanly" 1 "exit $exitcode"

# 10. Unusable state fails loudly rather than silently under-recording spend.
set +e
out=$(docker run --rm --user 65534:65534 -e LEDGER_FILE=/nonexistent-dir/spend.jsonl "$IMAGE" 2>&1)
rc=$?
set -e
[ "$rc" -ne 0 ] && grep -qi 'cannot create\|not writable' <<<"$out" \
  && check "unwritable state directory fails at startup" 0 || check "unwritable state directory fails at startup" 1 "rc=$rc $out"

# 11. The image carries a healthcheck a scheduler can use.
hc=$(docker inspect -f '{{if .Config.Healthcheck}}{{.Config.Healthcheck.Test}}{{end}}' "$IMAGE")
[ -n "$hc" ] && check "image defines a HEALTHCHECK" 0 || check "image defines a HEALTHCHECK" 1

echo
echo "== $pass passed, $fail failed =="
[ "$fail" -eq 0 ]
