# AGENTS.md — llm-router

Guidance for AI coding agents (and humans) working in this repository.
Read this before making changes; the README covers user-facing docs, this
file covers how the codebase is built, structured, and maintained.

## What this project is

`llm-router` is an OpenAI v1-compatible LLM router in Go (module `llm-router`,
Go 1.25). It exposes **logical models** (grouped by domain/app) backed by one
or more upstream **OpenAI** or **Azure OpenAI / Azure AI Foundry**
deployments, with automatic rerouting when a backend fails or is too slow to
start responding.

## Lineage

How this project evolved — useful context when deciding where a change
belongs:

1. **Core router** (initial build): logical models, reusable named
   credentials, two routing modes (`fallback`, `round_robin`), two distinct
   timeouts (`timeout` = overall budget, `reroute_timeout` = first-response
   deadline), upstream error pass-through (502), optional constant-time Bearer
   auth, Docker multi-stage build + Compose.
2. **Upstream transport on openai-go v3**: the official
   [`openai-go`](https://github.com/openai/openai-go) SDK (v3) is used for all
   upstream calls, with **SDK retries disabled** (`WithMaxRetries(0)`) —
   failover, rerouting, and timeouts are the router's job, so SDK-level
   retries would make semantics non-deterministic.
3. **Test infrastructure**: `internal/testutil` provides a fake
   OpenAI-compatible upstream (per-model behaviors: status, body, SSE events,
   delay) so unit tests run with no network. `e2e/` tests run against a real
   upstream (local vLLM by default) and **auto-skip** when unreachable.
4. **Observability** (most recent work):
   - **Prometheus metrics**: dedicated `prometheus.Registry` (never the
     process default), served at `GET /metrics` via `api.WithMetrics(...)`.
     The route is only registered when metrics are wired in (404 otherwise).
   - **Structured JSON logging to stdout**: previously logs went to stderr;
     they now go to stdout so container log drivers capture one clean stream.
     stderr must stay empty.
   - **Explicit startup failure handling**: config load, router init, and
     listen failures log an `ERROR` line to stdout and exit `1` (previously
     some paths used `log.Fatal`/implicit behavior).
   - Metrics tests in `internal/router/metrics_test.go` and
     `internal/api/api_test.go` (endpoint enabled + disabled).
5. **Secrets hygiene** (most recent): a vLLM API key had been hardcoded in
   the e2e tests, README, and AGENTS.md. It was removed from every file and
   replaced with environment-variable indirection:
   - `internal/envfile` loads `./.env` into the process environment at
     startup (and in the e2e `TestMain`). `.env` values **take priority**
     over system env; when `.env` is absent the system environment is used
     as-is.
   - `config.yaml` references only `${VAR}` names, never values.
   - `.env.example` is the committed skeleton; `.env` is gitignored.
   - E2E tests skip when `E2E_API_KEY` is unset instead of falling back to a
     baked-in key.
   - The exposed key must be considered compromised and rotated.

## Architecture

```
main.go                     entrypoint: flags, .env loading, slog JSON
                            logging, metrics, graceful shutdown (15s),
                            startup-failure exits
internal/config/            YAML config: ${ENV} expansion, validation,
                            per-model overrides of defaults
internal/provider/          thin openai-go v3 transport (openai | azure),
                            retries disabled — no failover logic here
internal/router/            routing engine: fallback / round_robin, reroute
                            timeout gating, cooldown tracking, health
internal/api/               OpenAI v1-compatible HTTP API: SSE pass-through,
                            model rewrite, OpenAI error shape, /metrics route
internal/metrics/           Prometheus collectors on a dedicated registry
internal/envfile/           .env loader (KEY=VALUE → process environment)
internal/testutil/          fake OpenAI-compatible upstream for tests
e2e/                        end-to-end tests against a real upstream
```

### Request flow

```
client → api.Server (auth, parse) → router.Model.Route()
       → provider.Client (openai-go, no retries) → upstream
       ← SSE/JSON pass-through with `model` rewritten to the logical name
```

- **Model rewrite**: the `model` field in every response (JSON and each SSE
  `data:` line) is rewritten to the **logical** model name so clients see a
  stable identity regardless of which backend served the request. Upstream
  requests carry the backend's real model name.
- **Thinking (reasoning) pass-through**: request parameters like
  `chat_template_kwargs` / `reasoning_effort` are forwarded unchanged, and
  response `reasoning` / `reasoning_content` fields (JSON message or SSE
  delta) pass through untouched — the router only rewrites `model`.
  Thinking models need a wide per-model `reroute_timeout` for non-streaming
  requests (see Timeout model below).
- **Backend identity**: backends are named `<upstream-model>@<credential>`
  (e.g. `qwen3.8-27b@vllm`). This string is used in logs, `/healthz`, and
  metric labels.
- **Routing modes**:
  - `fallback` — try backends in configured order; reroute on error or
    first-byte delay > `reroute_timeout`. Cooldown is recorded but not used
    for skipping.
  - `round_robin` — distribute across backends not in cooldown; cooling-down
    backends are used only as a last resort when all are cooling down.
- **Timeout model** (do not conflate):
  - `timeout` (default 120s) — total request budget; exceeding it =
    out-of-service (502). Bounds every attempt and the whole streaming body.
  - `reroute_timeout` (default 10s) — first-response deadline (headers +
    first body byte). Exceeding it, or any error, triggers a reroute. It is
    **not** the out-of-service timeout. For non-streaming requests the
    upstream sends headers and body together, so the **full** response must
    arrive within this window — thinking models can take 30–120s, so give
    them a wide per-model `reroute_timeout` (e.g. 120s). Streaming thinking
    is fine with defaults (first byte arrives quickly).
- **All backends failed**: `router.ErrAllBackendsFailed` carries
  `LastErrBody`; the API layer passes it through as a 502 when it is valid
  OpenAI error JSON, otherwise a generic 502 error body.
- **Auth**: optional; empty `server.api_key` disables it. Checked in constant
  time. `/healthz`, `/metrics`, and `/` do not require auth.

### Startup sequence (main.go)

1. `-config` flag (or `LLM_ROUTER_CONFIG` env, default `config.yaml`)
2. `slog` JSON handler on **stdout** (level INFO) + `slog.SetDefault`
3. `envfile.LoadDefault()` — load `./.env` if present (values override
   system env); missing file is not an error
4. `metrics.New()` → dedicated registry
5. `config.Load` (expands `${VAR}` from the merged environment) → on error:
   `log.Error` + `os.Exit(1)`
6. `router.NewRouter(cfg, router.WithMetrics(m))` → on error: exit 1
7. `api.New(r, apiKey, log, api.WithMetrics(m)).Handler()` on `http.Server`
   with read/write/idle timeouts from config
8. `ListenAndServe` in a goroutine; on bind error (not `ErrServerClosed`):
   `log.Error` + `os.Exit(1)`
9. SIGINT/SIGTERM → `srv.Shutdown` with 15s timeout

## Commands

Makefile targets:

| Command | What it does |
|---------|--------------|
| `make build` | Build binary to `./bin/llm-router` (`-trimpath`) |
| `make test` | Unit tests: `go test -timeout 90s ./...` (no network) |
| `make e2etest` | E2E tests: `go test -v -timeout 5m ./e2e/` (auto-skips if upstream unreachable) |
| `make run` | Build + run with `./config.yaml` |
| `make vet` | `go vet ./...` |
| `make fmt` | `gofmt -w .` |
| `make clean` | Remove `./bin` |
| `make docker` | Build image `llm-router:latest` |

Plain Go equivalents: `go build -o bin/llm-router .`, `go test ./...`,
`go test ./e2e/`.

E2E configuration (env vars, or a `.env` file in the working directory — the
e2e `TestMain` loads it; `.env` takes priority):

| Variable | Default |
|----------|---------|
| `E2E_BASE_URL` | `http://192.168.18.200:1235/v1` |
| `E2E_API_KEY` | *(required; tests skip when unset — never hardcode keys)* |
| `E2E_MODEL` | `qwen3.8-27b` |

The suite includes thinking tests (`TestE2E_Thinking_NonStreaming`,
`TestE2E_Thinking_Streaming`) that send a long prompt with
`chat_template_kwargs.enable_thinking: true` and assert substantial
`reasoning` output; they run with generous gates (reroute 120s, timeout
240s) and take ~1 minute against a live thinking model.

**Definition of done for any change**: `gofmt -l .` clean, `go vet ./...`
clean, `make test` green. Run `make e2etest` when upstream behavior is
touched (it skips itself when the upstream is down).

## Logging guidelines

- **JSON to stdout only.** One `slog` JSON object per line. Never write to
  stderr, never `fmt.Print*` — stderr must stay empty so container log
  drivers see a single clean stream.
- **Use the injected `*slog.Logger`** (constructor parameter). Internal
  packages do not create their own handlers; `main.go` owns the handler and
  sets the default.
- **Structured fields, not string interpolation**:
  `log.Warn("backend attempt failed", "model", m, "backend", b, "error", err)`
  — never `log.Warn("failed " + m)`.
- **Levels**:
  - `INFO` — lifecycle (listening, shutdown) and successful routing
    (`routed` with attempts + duration).
  - `WARN` — recoverable problems: a failed backend attempt, a reroute.
  - `ERROR` — critical failures: startup failures, shutdown failures.
- **Every error is either reported to the client or logged** — nothing is
  silently swallowed. If a write to the client fails, log it.
- **Startup failures** (config load, router init, listen/bind):
  `log.Error("startup failed: ...", "error", err)` then `os.Exit(1)`.
- Useful context fields already in use: `model`, `backend`, `stream`,
  `attempts`, `duration`, `addr`, `reason`, `path`.

## Metrics guidelines

- **Dedicated registry only.** All collectors register via `MustRegister` on
  the registry created by `metrics.New()`. Never use the process default
  registry (`prometheus.DefaultRegisterer`) — the router must not collide
  with anything else in the process.
- **Naming**: prefix `llm_router_`; counters end in `_total`; durations in
  seconds (`_seconds`).
- **Label conventions** (keep these exact — tests and dashboards depend on
  them):
  - `model` — logical model name
  - `mode` — `fallback` | `round_robin`
  - `stream` — `"true"` | `"false"` (string, via `streamLabel`)
  - `status` — `success` | `error`
  - `backend` — `<upstream-model>@<credential>`
  - `result` — `success` | `failure` | `timeout`
  - `reason` — `error` | `timeout`
- **Nil-safety**: every method on `*Metrics` is a no-op on a nil receiver.
  Callers pass metrics unconditionally; do **not** add `if m != nil` checks
  at call sites, and keep the nil check inside each method when adding new
  ones.
- **Current collectors** (see `internal/metrics/metrics.go`):
  - `llm_router_requests_total` (counter: model, mode, stream, status)
  - `llm_router_request_duration_seconds` (histogram: model, mode, stream;
    buckets tuned for LLM latencies: 50ms…300s)
  - `llm_router_backend_attempts_total` (counter: model, backend, result)
  - `llm_router_reroutes_total` (counter: model, reason)
  - `llm_router_backend_up` (gauge: model, backend; 0 = in cooldown)
  - `llm_router_inflight_requests` (gauge)
- **Adding a metric**: define the collector in `metrics.New()`, register it,
  add a nil-safe `Observe*`/`Set*` method, instrument the router (or API)
  call site, and assert it in a test.
- **Testing metrics**: use `promtestutil.ToFloat64` on counters/gauges
  (note: it **panics** on histograms — for histograms, gather from
  `Registry.Gather()` and inspect `GetHistogram().GetSampleCount()`). In
  `internal/router` tests, alias the import as `promtestutil` because
  `llm-router/internal/testutil` is also named `testutil`.
- **Exposure**: `GET /metrics` is registered only when
  `api.WithMetrics(...)` is passed (otherwise 404). No auth on `/metrics` —
  intended for local scraping.

## Testing guidelines

- **Unit tests** (`internal/...`): no network. Use
  `testutil.NewFake(t)` + `SetBehavior(model, Behavior{Status, Body, Events,
  Delay})` to simulate upstreams (errors, SSE streams, slow first byte).
  `testutil.ChatBody` / `ChatEvent` / `CompletionBody` build realistic
  payloads.
- **Router tests** build configs inline (see `buildRouter` in
  `router_test.go` / `buildMetricsRouter` in `metrics_test.go`); typical
  defaults: timeout 3s, reroute_timeout 200ms, cooldown 400ms.
- **API tests** serve the real `api.Server` through `httptest.Server`
  (see `newTestAPI` / `buildTestConfig` in `api_test.go`).
- **E2E tests** (`e2e/`): real upstream, non-streaming + streaming + models
  + round-robin distribution; each test skips when `E2E_API_KEY` is unset or
  the upstream is down. `TestMain` loads `.env` first.
- **Timing-sensitive tests**: keep reroute/cooldown windows short (hundreds
  of ms) and avoid sleeps longer than needed; the suite must stay well under
  the 90s unit-test budget.

## Secrets & environment guidelines

- **Never hardcode credentials** in source, tests, configs, or docs — not
  even "temporary" defaults. The one that leaked into `e2e_test.go` had to
  be scrubbed from four files and rotated.
- **Config files reference variable names only** (`${VAR}`), never values.
- **`.env` precedence**: when `./.env` exists, its values override system
  environment variables; when it is absent, the system environment is used
  as-is. `.env` is loaded once at startup (main.go) and in the e2e
  `TestMain`; it is not watched.
- **`.env` is gitignored**; `.env.example` is the committed skeleton — keep
  it in sync with every new variable the config or tests read.
- **E2E tests skip** when `E2E_API_KEY` is unset or the upstream is
  unreachable; they never fall back to a baked-in key.
- **Docker**: inject secrets via `-e`/Compose `environment` (or mount
  `.env` at `/app/.env` — note that a mounted `.env` overrides injected
  `-e` values by the precedence rule above).

## Conventions & gotchas

- **Do not add retries to the provider layer.** The router owns failover;
  SDK retries would double-attempt a backend and break the
  attempts/reroutes metrics semantics.
- **Keep the two timeouts separate.** Do not collapse `timeout` and
  `reroute_timeout` into one knob — the split is the core design decision
  (slow-but-working backends must not be rerouted; dead ones must be).
- **Model rewrite happens in the API layer**, not the router. The router
  returns the upstream response untouched (plus metadata); the API rewrites
  `model` in JSON and per SSE line.
- **Error bodies are capped** at 1 MiB (`maxErrBody`) when retained for
  pass-through.
- **Config**: `${ENV}` expansion happens at load time, after `.env` is
  loaded; per-model `timeout`/`reroute_timeout`/`cooldown` override
  `defaults`.
- **Docker**: multi-stage (`golang:1.25-alpine` → `alpine:3.20`), non-root
  user, `HEALTHCHECK` on `/healthz`. The image bakes in `config.yaml` at
  `/etc/llm-router/config.yaml` (`LLM_ROUTER_CONFIG` points there); override
  by mounting your own config read-only at that path.
- **Dependencies** (direct): `openai-go/v3`, `prometheus/client_golang`,
  `yaml.v3`. Keep the dependency set small; `go mod tidy` after adding
  imports.
