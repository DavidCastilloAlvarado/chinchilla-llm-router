# llm-router

An OpenAI v1-compatible LLM router in Go. It exposes **logical models** (grouped
by domain/app) that are backed by one or more upstream **OpenAI** or
**Azure OpenAI / Azure AI Foundry** deployments, with automatic rerouting when a
backend fails or is too slow to start responding.

Built on the official [`openai-go`](https://github.com/openai/openai-go) SDK
(v3) for upstream transport.

## Features

- **OpenAI v1-compatible API**: `POST /v1/chat/completions`,
  `POST /v1/completions`, `GET /v1/models`, plus `GET /healthz` and `GET /`.
  Streaming (SSE) and non-streaming responses are passed through, with the
  `model` field rewritten to the logical model name so clients see a stable
  identity no matter which backend served the request.
- **Logical models by domain/app**: one logical model can be backed by several
  upstream models/deployments across different providers.
- **Reusable credentials**: backends reference named credentials
  (`openai` or `azure`) instead of embedding secrets per backend.
- **Two routing modes**:
  - `fallback` — try backends in configured order; reroute on error or when the
    first response byte takes longer than `reroute_timeout`.
  - `round_robin` — distribute traffic across backends that have not recently
    failed/timed out; failing backends enter a cooldown and are skipped (used
    only as a last resort when all backends are cooling down).
- **Two distinct timeouts**:
  - `timeout` — the overall request budget; exceeding it means the service is
    out of service (502).
  - `reroute_timeout` — the first-response deadline that triggers rerouting.
    It is separate from, and typically much shorter than, the overall timeout.
- **Upstream error pass-through**: when every backend fails, the last upstream
  error body (OpenAI error JSON) is passed through to the client as a 502.
- **Thinking (reasoning) models**: reasoning fields (`reasoning` /
  `reasoning_content`) and thinking request parameters (`chat_template_kwargs`,
  `reasoning_effort`, …) pass through unchanged in both streaming and
  non-streaming responses. Thinking models need a wide per-model
  `reroute_timeout` for non-streaming requests (see [Timeouts](#timeouts)).
- **Optional client auth**: `Authorization: Bearer <key>` checked in constant
  time.
- **Prometheus metrics**: `GET /metrics` exposes request, attempt, reroute,
  backend-health, and in-flight gauges (see [Metrics](#metrics)).
- **Structured logging**: JSON logs to stdout (stderr stays clean for
  container log drivers); startup failures log the error and exit non-zero.
- **Docker**: multi-stage build, non-root runtime, healthcheck, Compose file.

## Installation

### One-liner (recommended)

`npx llm-router-cli` downloads the pre-built Go binary for your platform and
runs an interactive setup wizard that writes `config.yaml` + `.env` for you:

```sh
npx llm-router-cli
```

The wizard walks through: **server** (host, port, client auth) →
**credentials** (OpenAI, local OpenAI-compatible, or Azure) → **logical
models** (name, domain, routing mode, backends, optional per-model timeouts)
→ **defaults** (timeout / reroute timeout / cooldown) → summary → write.
Secrets are entered with hidden input and land only in `.env` (chmod 600);
`config.yaml` references them as `${VAR}`.

Everything installs under `~/.llm-router/` (override with `--dir` or
`$LLM_ROUTER_HOME`):

```
~/.llm-router/
├── bin/llm-router     the Go binary (PATH is updated in your shell rc)
├── config.yaml        generated config (${VAR} references only)
├── .env               your secrets (chmod 600)
├── downloads/         cached release tarballs
└── version.json       installed version + source URL
```

Then:

```sh
npx llm-router-cli run      # start the router
npx llm-router-cli doctor   # check binary, config validity, running server
npx llm-router-cli version  # print installer + binary versions
```

### Non-interactive setup

For scripts and CI, `init` accepts flags instead of the wizard:

```sh
npx llm-router-cli init \
  --cred vllm:local:http://192.168.18.200:1235/v1:sk-... \
  --model chat:chat:fallback:vllm:qwen3.8-27b \
  --model fast:chat:round_robin:vllm:qwen3.8-27b \
  --port 8080 --auth
```

- `--cred name:kind:url:key` — repeatable; `kind` is `openai`, `local`
  (OpenAI-compatible base URL), or `azure` (endpoint URL). The key must not
  contain `:`.
- `--model name:domain:mode:cred:upstream-model` — repeatable; `mode` is
  `fallback` or `round_robin`.
- `--auth` enables client auth (a key is auto-generated when `--api-key`
  is omitted); `--no-auth` disables it.
- `--timeout 120s --reroute-timeout 10s --cooldown 30s` set the defaults.

The generated config is validated by the router itself (`-check`) before the
command finishes.

### Other commands & flags

| Command | What it does |
|---------|--------------|
| `setup` (default) | Install the binary, then run the interactive wizard |
| `install` | Install the binary only (skips if already installed) |
| `init` | Generate config — interactively, or with the flags above |
| `doctor` | Check binary, config validity, and a running server |
| `run` | Run the installed router with the installed config |
| `version` | Print installer + installed binary versions |

Useful flags: `--dir <dir>` (install root), `--version <v>` (binary version),
`--force` (reinstall). Environment: `LLM_ROUTER_HOME` (install root),
`LLM_ROUTER_VERSION` (binary version), `LLM_ROUTER_RELEASE_BASE` (self-hosted
release base URL).

Supported platforms: Linux and macOS (Darwin), `amd64` and `arm64`. Windows
is not supported — use WSL or Docker. Requires Node.js ≥ 18 (for `npx`);
the installed router itself is a standalone Go binary.

### Building from source

A [Makefile](Makefile) provides the basic commands:

```sh
make build     # build the binary into ./bin
make test      # unit tests (fake upstream, no network)
make e2etest   # e2e tests against a real upstream (auto-skips if unreachable)
```

Other targets: `make run`, `make vet`, `make fmt`, `make clean`, `make docker`,
`make release VERSION=x.y.z` (cross-compiles linux/darwin × amd64/arm64
tarballs + `checksums.txt` into `./dist` — used by the
[GitHub release workflow](.github/workflows/release.yml), which publishes
them on `v*` tags for the `npx` installer to download).

The router binary also supports `-version` (print version) and `-check`
(validate the config file and exit without starting the server).

### Secrets: `.env`

Never hardcode credentials in code or config. Copy the skeleton and fill in
real values:

```sh
cp .env.example .env    # then edit .env (it is gitignored)
```

At startup the router loads `./.env` from the working directory **before**
expanding `${VAR}` references in the config file. Precedence:

1. `.env` values (when the file exists) — **take priority**
2. system environment variables (used as-is when `.env` is absent)

```sh
# Build (plain Go, or: make build)
go build -o bin/llm-router .

# Run (uses ./config.yaml by default; secrets come from .env / environment)
./bin/llm-router

# Or point at another config
./bin/llm-router -config /etc/llm-router/config.yaml
# (or: LLM_ROUTER_CONFIG=/etc/llm-router/config.yaml ./bin/llm-router)
```

Then use it like the OpenAI API:

```sh
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "chat-fast",
    "messages": [{"role": "user", "content": "Hello"}]
  }'

# Streaming
curl -N http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "chat-fast", "stream": true,
       "messages": [{"role": "user", "content": "Hello"}]}'
```

Any OpenAI SDK works by pointing `base_url` at the router:

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="...")
resp = client.chat.completions.create(model="chat-fast",
    messages=[{"role": "user", "content": "Hello"}])
```

## Configuration

See [`config.yaml`](config.yaml) for a fully commented example. Secrets are
referenced by environment variable name with `${VAR}` syntax — never by
value. At startup the router loads `./.env` (if present) into the environment
first (`.env` values take priority over system env), then expands the
`${VAR}` references when parsing the config. The full variable list lives in
[`.env.example`](.env.example).

```yaml
server:
  host: 0.0.0.0
  port: 8080
  api_key: ${ROUTER_API_KEY}      # optional; empty = auth disabled
  # read_timeout / write_timeout / idle_timeout: optional HTTP server timeouts

defaults:
  timeout: 120s          # overall request budget (out-of-service)
  reroute_timeout: 10s   # first-response deadline before rerouting
  cooldown: 30s          # round_robin: how long failed backends are skipped

credentials:
  openai-main:
    type: openai
    api_key: ${OPENAI_API_KEY}
    base_url: https://api.openai.com/v1   # optional
  azure-foundry:
    type: azure
    endpoint: ${AZURE_OPENAI_ENDPOINT}    # e.g. https://my-resource.cognitiveservices.azure.com
    api_key: ${AZURE_OPENAI_API_KEY}
    api_version: "2024-10-21"             # optional

models:
  - name: chat-fast
    domain: chat
    mode: fallback          # or round_robin
    # timeout / reroute_timeout / cooldown: optional per-model overrides
    backends:
      - credential: openai-main
        model: gpt-4o-mini
      - credential: azure-foundry
        model: gpt-4o-mini-deployment   # Azure deployment name
```

### Routing modes

| Mode | Behavior |
|------|----------|
| `fallback` | Backends are tried in configured order. On error, or if the first response byte does not arrive within `reroute_timeout`, the next backend is tried. Cooldown is recorded but not used for skipping. |
| `round_robin` | Requests are distributed across backends in round-robin order, skipping backends in cooldown (recently failed/timed out). If the chosen backend fails or is too slow, the request is rerouted to the next candidate. Cooling-down backends are used only when every backend is cooling down. |

### Timeouts

- **`timeout`** (default 120s): the total budget for the request. For
  non-streaming requests the full response must arrive within the reroute
  window of each attempt; for streaming requests the stream may continue up to
  the overall `timeout`, which cuts off runaway streams.
- **`reroute_timeout`** (default 10s): how long to wait for the first response
  byte (headers + first body byte) before rerouting to the next backend. This
  is deliberately separate from the overall timeout so a slow backend is
  rerouted quickly while a legitimately long generation is still allowed to
  complete.
- **Thinking models**: for non-streaming requests the upstream sends headers
  and body together, so a thinking response must arrive *in full* within
  `reroute_timeout` — and thinking responses can take 30–120s. Set a wide
  per-model `reroute_timeout` (and matching `timeout`) for thinking models,
  e.g. `reroute_timeout: 120s`. Streaming thinking works with the defaults
  because the first byte (the first reasoning delta) arrives quickly.

## API

| Endpoint | Description |
|----------|-------------|
| `POST /v1/chat/completions` | Chat completions (streaming + non-streaming) |
| `POST /v1/completions` | Legacy completions |
| `GET /v1/models` | Lists the configured **logical** models |
| `GET /healthz` | Liveness + per-backend health (cooldown state, last error) |
| `GET /metrics` | Prometheus metrics (see [Metrics](#metrics)) |
| `GET /` | Service info |

Errors use the OpenAI error shape:

```json
{"error": {"message": "...", "type": "invalid_request_error", "param": null, "code": null}}
```

When all backends fail, the router returns `502` and passes through the last
upstream error body when it is valid OpenAI error JSON.

## Metrics

`GET /metrics` exposes Prometheus metrics (no auth; intended for local
scraping or a sidecar/ingress that filters the path):

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `llm_router_requests_total` | counter | `model`, `mode`, `stream`, `status` (`success`/`error`) | Routed requests by logical model, mode, streaming and outcome |
| `llm_router_backend_attempts_total` | counter | `model`, `backend`, `result` (`success`/`failure`/`timeout`) | Individual backend attempts |
| `llm_router_reroutes_total` | counter | `model`, `reason` (`error`/`timeout`) | Reroutes to the next backend and why |
| `llm_router_backend_up` | gauge | `model`, `backend` | `1` if the backend is not in cooldown, `0` otherwise |
| `llm_router_inflight_requests` | gauge | — | Requests currently being routed |
| `llm_router_request_duration_seconds` | histogram | `model`, `mode`, `stream` | End-to-end routing duration |

`backend` is the `<model>@<credential>` pair. Useful alerts:

```promql
# Reroute rate (backend instability)
sum(rate(llm_router_reroutes_total[5m])) by (model)

# Backend down
llm_router_backend_up == 0

# Error rate
sum(rate(llm_router_requests_total{status="error"}[5m])) by (model)
```

## Logging

Logs are structured JSON on **stdout** (one object per line); stderr is left
empty so container log drivers capture a single clean stream. Every log line
includes a timestamp, level, and message, plus context fields:

```json
{"time":"...","level":"INFO","msg":"llm-router listening","addr":"0.0.0.0:8080","models":2,"endpoints":[...]}
{"time":"...","level":"WARN","msg":"backend attempt failed","model":"chat-fast","backend":"gpt-4o@openai-main","stream":false,"error":"..."}
{"time":"...","level":"INFO","msg":"routed","model":"chat-fast","backend":"gpt-4o-mini@azure","stream":true,"attempts":2,"duration":301341707}
```

Startup failures (invalid config, router construction error, bind failure)
log an `ERROR` line with the cause and exit with status `1`.

## Testing

```sh
# Unit tests (fake upstream, no network needed)
make test            # or: go test ./...

# E2E tests against a real OpenAI-compatible upstream (local vLLM by default).
# Skipped automatically when the upstream is not reachable.
make e2etest         # or: go test -v -timeout 5m ./e2e/
```

E2E configuration via environment variables (or a `.env` file in the working
directory — the e2e suite loads it too):

| Variable | Default |
|----------|---------|
| `E2E_BASE_URL` | `http://192.168.18.200:1235/v1` |
| `E2E_API_KEY` | *(required; tests skip when unset — never hardcode keys)* |
| `E2E_MODEL` | `qwen3.8-27b` |

The suite includes thinking (reasoning) tests — non-streaming and streaming
with a long prompt — which take ~1 minute against a live thinking model.

## Docker

```sh
# Build the image
docker build -t llm-router:latest .

# Run with Compose (mounts ./.env into the container; all variables
# — upstream credentials, router key, ... — come from that one file)
docker compose up -d

# Or run directly
docker run -d --name llm-router -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/llm-router/config.yaml:ro" \
  -v "$PWD/.env:/app/.env:ro" \
  llm-router:latest
```

The image is multi-stage (`golang:1.25-alpine` → `alpine:3.20`), runs as a
non-root user, and includes a `HEALTHCHECK` against `/healthz`.

## Project layout

```
main.go                     entrypoint (flags, .env loading, logging,
                            graceful shutdown)
AGENTS.md                   agent/human contributor guide (architecture,
                            commands, logging & metrics guidelines)
Makefile                    build/test/e2e/run targets
config.yaml                 example configuration (${VAR} references only)
.env.example                skeleton of all environment variables
.env                        your local secrets (gitignored)
internal/config/            YAML config types, ${ENV} expansion, validation
internal/provider/          OpenAI / Azure client wrapper (openai-go SDK)
internal/router/            routing core: fallback, round-robin, reroute
                            timeout gating, backend health/cooldown
internal/api/               OpenAI v1-compatible HTTP API (SSE, model rewrite)
internal/metrics/           Prometheus metrics (requests, attempts, reroutes,
                            backend health, inflight, duration)
internal/envfile/           .env loader (KEY=VALUE → process environment)
internal/testutil/          fake OpenAI-compatible upstream for tests
e2e/                        end-to-end tests against a real upstream
installer/                  npx installer (llm-router-cli): binary download,
                            interactive setup wizard, doctor, run
.github/workflows/release.yml  cross-compile + GitHub Release publishing
dist/                       release artifacts (make release output, gitignored)
Dockerfile, docker-compose.yml, .dockerignore, .gitignore
```
