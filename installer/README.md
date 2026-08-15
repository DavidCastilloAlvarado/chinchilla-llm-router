# chinchilla-llm-router

Installer for [llm-router](https://github.com/DavidCastilloAlvarado/chinchilla-llm-router) —
an OpenAI v1-compatible LLM router with logical models, fallback/round-robin
routing, and automatic rerouting.

This package is a zero-dependency Node.js bootstrap (Node ≥ 18). It downloads
the prebuilt Go binary for your platform from
[GitHub Releases](https://github.com/DavidCastilloAlvarado/chinchilla-llm-router/releases),
verifies it (sha256), installs it under `~/.llm-router/`, and generates
`config.yaml` + `.env` for you. The router itself is a standalone Go binary —
Node is only used to install and launch it.

## Quick start

```sh
npx chinchilla-llm-router
```

Interactive wizard: server (host/port/auth) → credentials (OpenAI, local
OpenAI-compatible, Azure) → logical models → defaults → writes config.
Secrets go only to `.env` (chmod 600); `config.yaml` uses `${VAR}` references.

Then:

```sh
npx chinchilla-llm-router run      # start the router in the background (returns immediately)
npx chinchilla-llm-router status   # is it running?
npx chinchilla-llm-router stop     # stop it
npx chinchilla-llm-router doctor   # check binary, config, running server
npx chinchilla-llm-router version  # installer + binary versions
```

`run` starts the router **detached** and returns as soon as `/healthz`
answers; running it again restarts the server (reboot). `run --foreground`
blocks the terminal instead (for debugging). A detached server logs JSON
lines to `~/.llm-router/logs/llm-router.log`.

## Non-interactive (scripts / CI)

```sh
npx chinchilla-llm-router init \
  --cred vllm:local:http://192.168.18.200:1235/v1:sk-... \
  --model chat:chat:fallback:vllm:qwen3.8-27b \
  --port 8080 --auth
```

## Commands

| Command | What it does |
|---------|--------------|
| `setup` (default) | Install the binary, then run the wizard |
| `install` | Install the binary only (skips if present) |
| `init` | Generate config — wizard or flags |
| `doctor` | Check binary, config validity, running server |
| `run` | Start the router detached (restarts it first if already running); `--foreground` blocks the terminal |
| `stop` | Stop the detached router |
| `status` | Show whether the detached router is running |
| `version` | Print installer + installed binary versions |

Flags: `--dir <dir>` (install root), `--version <v>` (binary version),
`--force` (reinstall), `--foreground` (with `run`). Environment: `LLM_ROUTER_HOME`, `LLM_ROUTER_VERSION`,
`LLM_ROUTER_RELEASE_BASE` (self-hosted releases).

Supported: Linux and macOS, amd64 and arm64. Windows: use WSL or Docker.

Full documentation: <https://github.com/DavidCastilloAlvarado/chinchilla-llm-router#installation>
