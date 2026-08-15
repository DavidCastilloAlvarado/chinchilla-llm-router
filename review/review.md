# llm-router — Code Review

- **Project**: `/home/david/workspace/david/qwen38_demo/llm-router`
- **Date**: 2026-08-15
- **Reviewer toolchain**: `go version go1.25.0 linux/amd64`
- **Scope**: full source tree (`main.go`, `internal/{api,config,envfile,metrics,provider,router,testutil}`, `e2e/`), config, Docker, Makefile, README, AGENTS.md
- **Note**: the directory is **not a git repository** (`git rev-parse` → `fatal: not a git repository`), so `.gitignore` provides no VCS protection today; findings below treat files as on-disk artifacts.

## Severity scale

| Tag | Meaning |
|-----|---------|
| **P1** | Must fix before exposing the router beyond a trusted network / before production. |
| **P2** | Should fix: meaningful correctness, cost, or operational issue (workaround or documentation may exist). |
| **P3** | Nice to fix: hardening, hygiene, minor issues. |

## Verdict

Well-structured, idiomatic Go with a clean layering (`main → api → router → provider`), a carefully reasoned two-timeout model, correct concurrency (all `health` state is mutex-guarded; `go test -race` clean), bounded request bodies, nil-safe metrics, constant-time auth comparison, and a genuinely good test story (behavior-scripted fake upstream + auto-skipping e2e against a real vLLM). `go vet` and `gofmt` are clean.

The issues that matter are at the **edge of the system**: what happens when an upstream answers with a 4xx, what the defaults do when `ROUTER_API_KEY` is forgotten, and what the observability endpoints expose. Nothing here is a crash or a memory-safety bug; the P1 is an insecure-by-default exposure surface, and the P2s are semantics/cost footguns that are partially documented.

---

## Findings

### P1-1 — Insecure-by-default exposure: `0.0.0.0` bind + silently disabled auth + no rate limiting

**Files**: `config.yaml` (`server:` block), `internal/config/config.go:81-90` (`Server.Addr()`), `internal/api/api.go:80-84` (`authorized()`), `main.go:76`

The default deployment is an unauthenticated, unrate-limited LLM proxy on all interfaces:

1. `Server.Addr()` defaults to `0.0.0.0:8080` when `host` is unset.
2. `config.yaml` sets `api_key: ${ROUTER_API_KEY}`. If `ROUTER_API_KEY` is unset, `ExpandEnv` (`internal/config/config.go:157-162`) expands it to `""`, and `authorized()` returns `true` for every request when `s.apiKey == ""` — auth is **silently disabled** with no warning log.
3. There is no rate limiting, no per-client quota, and a single shared key for all clients, so one forgotten env var turns the router into an open credit-burning proxy for anyone who can reach port 8080.

The behavior is documented ("empty = auth disabled"), but the *combination* of defaults is fail-open.

**Fix**: at minimum, log a loud warning at startup when `api_key` is empty and `host` is `0.0.0.0`; better, default `host` to `127.0.0.1` and require an explicit opt-in to bind wide, or require an explicit `auth: disabled` config key to turn auth off.

---

### P2-1 — Upstream 4xx responses are reroutable and masked as `502`

**Files**: `internal/provider/provider.go:77-82` (`Client.Do`), `internal/router/router.go:328-356` (`routeFallback`), `internal/router/router.go:364-409` (`routeRoundRobin`), `internal/api/api.go:259-274` (`writeRouteError`)

`Client.Do` returns a non-nil `err` for **any** upstream status ≥ 400, and both routing modes reroute on any `err != nil`. Consequences:

- A malformed client request (`400`) is retried against **every** backend — N× the upstream cost and latency for a request that was never going to succeed.
- A bad upstream credential (`401`/`403`) looks like a router failure: the operator sees `502` after the full reroute walk instead of an immediate "credential rejected".
- `writeRouteError` always answers `502` (passing the upstream body through). OpenAI-compatible clients/SDKs treat 4xx as terminal and 5xx as retryable, so the masked status can trigger client-side retries that amplify the waste above.

**Fix**: classify by upstream status in `attempt`/`Do` — `429`/`408`/`5xx`/timeout → reroute; other 4xx → stop and pass the response through with its **original** status code and body.

---

### P2-2 — `/healthz`, `/metrics`, and `/` are unauthenticated

**Files**: `internal/api/api.go:65-75` (`Handler()`)

Only the `/v1/*` routes call `authorized()`. The observability routes are open:

- `GET /healthz` exposes per-backend names, `last_error` (raw upstream error text, which can include provider/resource details), and `fails`/`hits` counters.
- `GET /metrics` exposes logical model names, per-backend attempt/reroute/error stats, and durations.
- `GET /` exposes the domain→model map.

The README documents `/metrics` as "intended for local scraping or a sidecar/ingress that filters the path", but with the default `0.0.0.0` bind (see P1-1) these are reachable from the network by default.

**Fix**: put the three routes behind the same Bearer check, or make network-level path filtering an explicit, documented deployment requirement.

---

### P2-4 — Non-streaming requests are gated on the *full body* by `reroute_timeout` (default 10s)

**Files**: `internal/router/router.go:458-465` (`attempt`, non-streaming branch), `config.yaml` (`reroute_timeout: 10s`)

For non-streaming requests the upstream sends headers and body together, so the full response must arrive within the reroute gate. A non-streaming thinking-model request that takes >10s is treated as a reroute timeout: the in-flight generation is abandoned and the request is regenerated on the next backend — duplicate work and duplicate cost, and with `fallback` mode every backend can generate the same answer before one finishes.

This is documented (README "Timeouts" section; `config.yaml` ships a `think` example with `reroute_timeout: 120s`), but the *default* remains a trap for any non-streaming model whose first full response exceeds 10s.

**Fix**: for non-streaming attempts, gate only on headers + first byte (as streaming does) and let the overall `timeout` bound the body; or at least warn at startup when a model's backends are likely thinking models.

---

### P3-1 — Per-backend `api_version` is parsed but never used (dead config field)

**Files**: `internal/config/config.go:46-47` (`Backend.APIVersion`, "optionally overrides the credential's Azure api-version"), `internal/router/router.go:204` (`NewModel` calls `provider.New(cred)` with the credential only)

`config.Backend.APIVersion` is unmarshalled from YAML but never read; only `Credential.APIVersion` reaches `provider.New` (`internal/provider/provider.go:52`). A user who sets `api_version` on a backend gets a silent no-op.

**Fix**: wire the per-backend override through (per-backend client or an option on `provider.New`), or remove the field.

### P3-2 — No TLS, rate limiting, or per-client accounting

Plain HTTP only; a single shared Bearer key; no per-client quota or rate limit. Acceptable for an internal router behind an ingress, but should be an explicit deployment requirement (see P1-1).

### P3-3 — Graceful shutdown caps in-flight streams at 15s

**File**: `main.go:106` — `srv.Shutdown` with a `15*time.Second` deadline. Long in-flight streams (thinking models can run for minutes) are cut off on SIGTERM. Make the drain deadline configurable or document it.

### P3-4 — Server timeouts default to zero (no slow-client protection)

**Files**: `internal/config/config.go:73-75`, `main.go:77-79`, `config.yaml` (no `read_timeout`/`write_timeout`/`idle_timeout` set)

Unset durations are `0`, so `http.Server` gets no `ReadTimeout`/`WriteTimeout`/`IdleTimeout`/`ReadHeaderTimeout`. A slow client can hold connections open indefinitely (slowloris-style connection exhaustion). Zero `WriteTimeout` is *correct* for long streams, but the read/idle side should have sane defaults (e.g. `ReadHeaderTimeout: 10s`, `IdleTimeout: 60s`).

### P3-5 — e2e defaults hardcode a LAN IP

**File**: `e2e/e2e_test.go` — `defaultBaseURL = "http://192.168.18.200:1235/v1"`, `defaultModel = "qwen3.8-27b"`. Auto-skip keeps CI green, but a hardcoded private IP in source is a smell; make the default `127.0.0.1` and require `E2E_BASE_URL`/`.env` for the lab endpoint.

### P3-6 — `.env` overrides the process environment (documented, but surprising)

**File**: `internal/envfile/envfile.go` — `Load` calls `os.Setenv` unconditionally, so `.env` wins over injected `-e`/system env vars. This is the opposite of the common dotenv convention and is documented (README, AGENTS.md, Dockerfile comment), but it will surprise anyone who expects `docker run -e` to override the baked-in `.env`.

### P3-7 — `ExpandEnv` expands bare `$VAR` as well as `${VAR}`

**File**: `internal/config/config.go:157-162` — `os.Expand` also expands `$VAR` without braces; a config value containing a literal `$` (a model name, a prompt fragment) can be silently rewritten. Use a `${VAR}`-only expansion.

### P3-8 — README/code mismatch on error pass-through

README: "passes through the last upstream error body **when it is valid OpenAI error JSON**". Code (`internal/api/api.go:262-265`): passes through **any** non-empty body with `Content-Type: application/json`, no validation. Either validate the JSON or fix the README.

### P3-9 — SSE scanner line cap is 4 MiB

**File**: `internal/api/api.go` (`writeSSE`) — `scanner.Buffer(..., 4<<20)`. A single SSE line longer than 4 MiB aborts the stream (with a visible `stream interrupted` notice). Edge case, but worth a comment or a larger cap.

### P3-10 — No global concurrency cap (bounded-per-request memory amplification)

Request bodies are correctly capped at 10 MiB (`internal/api/api.go:31,101` — `maxRequestBody` + `io.LimitReader`), but there is no in-flight request cap; N concurrent clients × 10 MiB buffered = unbounded memory. Minor for an internal router; a semaphore or `http.Server`-level limit would close it.

### P3-11 — Build artifact in the tree

`bin/llm-router` — a ~22 MB ELF x86-64 binary (with debug info, not stripped) present on disk. Gitignored, so VCS-clean; `make clean` removes it. Hygiene only.

### P3-12 — Real upstream credential stored in `.env` on disk (local hygiene only)

**File**: `.env`

`.env` sets `VLLM_API_KEY`, `E2E_API_KEY`, and `ROUTER_API_KEY` to real-looking values. That is the intended and correct place for them, and the mitigations are solid: `.gitignore` excludes `.env`, this directory is not a git repository, and `.dockerignore` excludes `.env` from the image — so the secret is never committed or baked into the image. The only residual risk is local: the file is mode `644`, and any backup/sync of this directory would carry the key. (The actual values are intentionally not reproduced in this document.)

**Fix (optional)**: `chmod 600 .env`; avoid copying the file to shared storage.

---

## Architecture

**Layering and dependencies.** Clean, acyclic, and correctly directed:

```
main.go ──> internal/api ──> internal/router ──> internal/provider ──> openai-go SDK
   │            │                 │
   ├──> internal/config <────────┘
   ├──> internal/envfile
   └──> internal/metrics (prometheus, dedicated Registry, nil-safe)
internal/testutil (fake upstream) ── used only by tests
```

- `internal/api` owns the HTTP surface (auth, body limits, SSE pass-through, error shaping); `internal/router` owns routing policy (fallback/round-robin, cooldowns, the reroute gate); `internal/provider` is a thin transport wrapper. No layer reaches across another's abstraction.
- The **logical model / credential / backend** split is the right abstraction: reusable named credentials, per-backend model names, per-domain logical models. `Router` is immutable after construction (read-only under `RWMutex`); there is no hot reload — a P3 note, not a defect for this scope.
- **Two-timeout model** (`timeout` vs `reroute_timeout`) is well reasoned and unusually well documented; the non-streaming asymmetry is the one sharp edge (P2-4).
- **Streaming pass-through** re-encodes each SSE `data:` event to rewrite the `model` field. Correct and necessary for the stable-identity contract; per-event JSON round-trips are a modest CPU cost (P3, acceptable).
- **State** (cooldowns, counters) is in-memory only; a restart resets backend health. Fine for this scope — no persistence needed.
- **openai-go as raw transport**: `Execute` with `option.WithResponseInto` and `WithMaxRetries(0)` on both clients is a deliberate, well-commented choice — it disables SDK retries so failover stays deterministic and the router owns all retry policy. Good call; it does mean the SDK's convenience layer is bypassed, so `provider.go` must keep up with SDK transport changes.

**What I verified is *not* a problem** (checked and cleared):

- **No data race in health tracking.** The `health` struct (`internal/router/router.go:99-136`) guards `cooldownEnd`/`lastErr`/`fails`/`hits` with a mutex; `inCooldown`, `markFailed`, `markHealthy`, `markServed`, and `Statuses` all acquire it. `rrIndex` is `atomic.Uint64`. Confirmed clean under `go test -race -count=1`.
- **Request bodies are bounded** at 10 MiB via `io.LimitReader` (`internal/api/api.go:31,101`); oversized bodies fail JSON unmarshal with a 400.
- **Metrics are nil-safe** (every `*Metrics` method no-ops on a nil receiver), so `Route`'s `InflightInc/Dec` is safe when metrics are disabled.
- **Context lifecycle** in `attempt` is carefully managed (`cancelOnClose`, ownership transfer of the attempt context, drain goroutine on the reroute-timeout path) with excellent explanatory comments.

## Security

**Good:**

- Secrets flow through `${VAR}` expansion only; `config.yaml` contains no secrets; `.env` is gitignored and dockerignored (not baked into the image).
- Auth check uses `subtle.ConstantTimeCompare` (`internal/api/api.go:80-96`).
- Bounded request body (10 MiB, `internal/api/api.go:31,101`) and bounded upstream error capture (1 MiB, `internal/router/router.go:38,533`).
- Docker image runs as non-root (uid 10001), multi-stage, static binary, with a healthcheck.
- No secrets in logs; structured JSON logging to stdout; error strings are sanitized before being embedded in SSE notices (`sanitizeMsg`).

**Issues** (see findings):

- P1-1: fail-open auth + `0.0.0.0` default + no rate limiting.
- P2-2: unauthenticated `/healthz`, `/metrics`, `/` leak topology and raw upstream error text (`last_error` can carry provider details).
- P3-12: real upstream credential in `.env` (correctly gitignored/dockerignored; local hygiene only).
- P3-2/P3-4: no TLS in-process (expected behind an ingress), no slow-client timeout protection.
- Note: upstream error bodies are passed through verbatim (P2-1), which can surface provider-specific details to clients; acceptable for an internal router, worth knowing.

## Code Quality

**Strengths**

- Idiomatic, small, focused packages; every package and most functions carry real doc comments, including *why* comments on the tricky bits (reroute gate, context ownership, SDK retry disabling).
- Error handling is consistent: wrapped errors, `errors.Is`/`errors.As`, no silent swallowing — `main.go` documents the explicit policy ("log and continue" vs "log and exit").
- Concurrency is correct and conservative (see "verified not a problem" above).
- Tests are a strength: table-driven unit tests against a behavior-scripted fake upstream (`internal/testutil/fake.go` records hits, auth headers, last request), e2e against a real upstream with auto-skip, and a race-clean suite. `TestFallback_ReroutesOnUpstreamError` and the e2e streaming tests cover the interesting paths.
- `go vet` and `gofmt` clean; minimal direct dependencies (`openai-go/v3`, `prometheus/client_golang`, `yaml.v3`).
- `Makefile`, `Dockerfile`, `docker-compose.yml`, `AGENTS.md`, and README are coherent and match the code (mostly — see P3-8).

**Weaknesses / nits**

- P3-1 dead config field (`Backend.APIVersion`) and P3-8 README/code mismatch are the two "documentation lies" — both cheap to fix.
- `handleIndex` returns 404 for any path other than `/` under the `GET /` registration — fine, but a catch-all 404 handler would be clearer.
- No request/correlation ID in logs (P3, ops ergonomics).
- `e2e` package mixes harness helpers and tests; fine at this size.

## Verification evidence (run 2026-08-15)

```
$ go version
go version go1.25.0 linux/amd64

$ go vet ./...            # exit 0, no output
$ gofmt -l .              # no output (clean)

$ go test -timeout 180s ./...
?   llm-router                  [no test files]
ok  llm-router/e2e              60.010s
ok  llm-router/internal/api     0.534s
ok  llm-router/internal/config  0.011s
ok  llm-router/internal/envfile 0.006s
?   llm-router/internal/metrics [no test files]
?   llm-router/internal/provider [no test files]
ok  llm-router/internal/router  2.131s
?   llm-router/internal/testutil [no test files]

$ go test -race -count=1 -timeout 300s ./internal/...
ok  llm-router/internal/api     1.609s
ok  llm-router/internal/config  1.026s
ok  llm-router/internal/envfile 1.017s
ok  llm-router/internal/router  3.214s
```

(e2e ran against the live upstream at `http://192.168.18.200:1235/v1`, model `qwen3.8-27b`.)

## Recommended priority order

1. **P1-1** — fail-closed auth default or loud startup warning (1-line change + docs).
2. **P2-1** — status-classified rerouting + pass-through of original 4xx status (contained in `provider.Do`/`router.attempt`/`api.writeRouteError`).
3. **P2-2** — auth on `/healthz`, `/metrics`, `/` (or documented network restriction).
4. **P2-4** — first-byte-only gate for non-streaming attempts (or startup warning).
5. P3 batch — dead `api_version` field, README fix, server read/idle timeout defaults, shutdown drain config, e2e default IP, `chmod 600 .env`.
