# Contributing

Thanks for wanting to help. Bifrost is deliberately small — a single stdlib-only
Go binary — so the bar to contribute is low.

## Build & test

Prerequisites: Go (see [`go.mod`](../go.mod) for the required version). No other
dependencies — the standard library is a hard rule, so the build needs no
`go get`.

```bash
gofmt -w .          # format
go vet ./...        # static checks
go build ./...      # build
go test ./...       # run the full suite
```

Tests spin up in-memory `httptest` backends — no network, no keys required.

## Design principles

- **Stdlib-only.** No third-party dependencies. If a feature needs a library,
  the bar is "why can't this be ~50 lines of stdlib?"
- **Stateless.** Behavior is driven by environment config. The only state is an
  optional append-only ledger and in-memory caches (never persisted).
- **Generic backends.** No provider is hardcoded in the source — backends,
  routes, and fallbacks are JSON config. Provider-specific logic (like DeepSeek
  pricing) lives in small, clearly-marked blocks and is overridable via config.
- **One pipeline.** Every request flows through `resolve()` in
  [`resilience.go`](../resilience.go), so routing, key rotation, retries, and
  failover are guaranteed to behave identically across every endpoint.

## Where things live

See the [code layout](ARCHITECTURE.md#code-layout) table in the architecture doc.

## Conventions

- Each source file has a matching `_test.go` with focused unit tests, plus a
  reset hook in [`setup_test.go`](../setup_test.go) for any package-level state.
- New observable state gets a Prometheus metric; new metrics get a test.
- New config gets a row in [`CONFIGURATION.md`](CONFIGURATION.md) and a bullet
  in the [roadmap](ROADMAP.md).

## Commit & PR

Branch off `main`, keep commits focused, and make sure `go vet ./...` and
`go test ./...` are green before opening a PR. CI runs the same on every push.
