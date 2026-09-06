# garess

[See it in action](https://github.com/user-attachments/assets/adcdeb3d-a0f8-461c-b31a-e35855f69bc4)

A minimal AI harness for the terminal. It talks to any OpenAI-compatible
`/v1/chat/completions` endpoint — local [llama.cpp], DeepSeek, OpenRouter, … —
through a Claude-Code-style TUI with an agentic tool loop, project + global
memory, git-style shell hooks, and `AGENTS.md`/`SYSTEM.md` support.

It is built to run well on a **Raspberry Pi 1** (32-bit ARMv6, ~700 MHz, 426 MB
RAM): pure-Go, statically linked, cross-compiled with `GOARM=6`, and with the
rendering pipeline engineered around the Pi's limits. It runs just as well on a
desktop.

[llama.cpp]: https://github.com/ggerganov/llama.cpp

## What it does

- **Agentic tool loop** — the model can run `bash`, read/write files, glob,
  grep, and manage memory notes, each gated by an allow/ask/deny policy
  (deny > ask > allow). `ask` tools request a human `y`/`n` in the TUI
  (ADK HITL round trip).
- **Git-style shell hooks** — commands run on agent/tool/model/session
  events; blocking hooks abort the operation on non-zero exit.
- **Landlock write sandbox** (opt-in) — confines tool _writes_ to the project
  while leaving reads and execution unrestricted.
- **Memory notes** in two scopes: project (`.garess/memory/`) and global
  (`~/.config/garess/memory/`).
- **`AGENTS.md` + `SYSTEM.md` + skills** — project and user instruction files
  are injected into the model context on every turn; `@import` and `{{VAR}}`
  are expanded.
- **JSONL session transcripts** — every completed event is persisted to
  `.garess/sessions/` for review and replay.
- **Thinking support** — model reasoning is captured and stored but hidden by
  default; press `ctrl+t` to show or hide it.
- **`garess doctor`** validates config, endpoint connectivity, hooks, and
  sandbox support.

## Quick start

Three ways to run `garess` — pick one:

**Download a binary** (no tooling). Every
[GitHub Release](https://github.com/alexewerlof/garess/releases) attaches
static single-file binaries for Linux, macOS, Windows and FreeBSD;
`garess-linux-armv6` is the Raspberry Pi 1 build. Linux example:

```sh
curl -sL -o garess https://github.com/alexewerlof/garess/releases/latest/download/garess-linux-amd64
chmod +x garess
./garess init -g                       # create ~/.config/garess/config.toml
$EDITOR ~/.config/garess/config.toml   # set your provider/endpoint
./garess doctor                        # verify config + endpoint
./garess                               # start chatting
```

**Run the container image** (Linux, `docker` or `podman`):

```sh
docker run -it --rm \
  -v ~/.config/garess:/home/garess/.config/garess \
  -v "$PWD:/work" -w /work \
  ghcr.io/alexewerlof/garess
```

**Build from source** (Go ≥ 1.26):

```sh
git clone <this repo> && cd garess
make build                          # -> dist/garess
./dist/garess init -g               # create ~/.config/garess/config.toml
$EDITOR ~/.config/garess/config.toml   # set your provider/endpoint
./dist/garess doctor                # verify config + endpoint
./dist/garess                       # start chatting
```

On a Raspberry Pi 1: `make build-arm` produces `dist/garess-linux-armv6`
(static, no Go needed on the Pi); copy it over and run it there.

Per-platform files, checksum verification, and the full walkthrough:
**[docs/getting-started.md](docs/getting-started.md)**. Cutting a release
(binaries + container image) is a single tagged push — see
**[docs/releasing.md](docs/releasing.md)**.

## Documentation

Two audiences, two paths — all docs live in [`docs/`](docs/):

### End users (getting work done)

| Doc                                        | What it covers                                                       |
| ------------------------------------------ | -------------------------------------------------------------------- |
| [Getting started](docs/getting-started.md) | Install, configure, first chat                                       |
| [Using the TUI](docs/usage.md)             | Keys, slash commands, memory notes, sessions, AGENTS.md/skills       |
| [Configuration](docs/configuration.md)     | Full TOML reference (providers, hooks, sandbox, env vars)            |
| [Safety model](docs/safety.md)             | Tool approvals, hooks, and the sandbox — what is and isn't protected |

### Developers (hacking on garess)

| Doc                                  | What it covers                                         |
| ------------------------------------ | ------------------------------------------------------ |
| [Architecture](docs/architecture.md) | Packages, request flow, design decisions               |
| [Development](docs/development.md)   | Build, test, cross-compile, profiling on slow hardware |

### AI agents

Every source directory has an `AGENTS.md` describing that package's
conventions and gotchas — the file at the repo root is the entry point, and
each package's `AGENTS.md` is authoritative for its folder. If you are an AI
coding agent working in this repo, start at [`AGENTS.md`](AGENTS.md).

## Requirements

- Go ≥ 1.26 to build (pure-Go dependencies, `CGO_ENABLED=0` — nothing else).
- A TTY for the TUI.
- An OpenAI-compatible endpoint with a `/v1` prefix.
- The agentic tools and sandbox are Linux-oriented (Landlock is Linux); the
  binary is built and tested on Linux and cross-compiled for Linux/ARMv6.

## Status

All five roadmap phases are implemented and validated on-device on a
Raspberry Pi 1: agentic tool loop, hooks, Landlock sandbox, and a full
agentic session against a LAN llama.cpp. Details and a note on the fixes that
came out of Pi validation are in [`docs/development.md`](docs/development.md)
and the root `AGENTS.md`.
