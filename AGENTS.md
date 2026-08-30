# AGENTS.md

garess is a minimal AI harness written in Go: an OpenAI-compatible chat client
with a Claude-Code-style TUI, project/global memory, JSONL session transcripts
and AGENTS.md/SYSTEM.md support. Model "thinking" (reasoning) is captured and stored but
hidden by default — press `ctrl+t` (as in Pi) to show or hide it. It
cross-compiles to a static 32-bit ARMv6 binary for a Raspberry Pi 1.

This file is read by garess itself: running `garess` from this directory
injects it into the model context as a system message (see
`internal/agents/`). The project search is capped at the launch directory.

## Commands

```sh
make build       # host binary -> dist/garess
make build-arm   # RPi 1 cross-compile -> dist/garess-linux-armv6 (GOARM=6)
make test        # go test ./...
make vet
make run         # go run ./cmd/garess
make doctor      # config + endpoint + sandbox checks
```

Finish changes with `go build ./... && go test ./...`; keep `gofmt` clean
(`make fmt`). Do not commit build output (`dist/`) or local state
(`.garess/`, `config.toml` at root — gitignored).

## Layout

| Path               | Purpose                                                                               |
| ------------------ | ------------------------------------------------------------------------------------- |
| `cmd/garess/`      | Entry point, flags, `doctor` subcommand, friendly config errors                       |
| `internal/agents/` | AGENTS.md + SYSTEM.md discovery + rendering (`@import` imports, env-var substitution) |
| `internal/chat/`   | Sessions + JSONL transcripts (`.garess/sessions/`)                                    |
| `internal/config/` | TOML config, XDG paths, merge, typed errors                                           |
| `internal/llm/`    | OpenAI-compatible provider abstraction                                                |
| `internal/memory/` | Local + global memory notes                                                           |
| `internal/tui/`    | Bubble Tea UI (model, streaming, markdown, commands)                                  |

Each package has its own `AGENTS.md` with its specific conventions — read the
one in the folder you are editing.

## Stack

- Go module `garess`; pure-Go dependencies only (`CGO_ENABLED=0` for the RPi
  build).
- **Charm stack is v1**: bubbletea v1.3.10, bubbles v1.0.0, lipgloss v1.1.0,
  glamour v1.0.0 (`github.com/charmbracelet/*` paths). Do NOT "upgrade" to v2 —
  `bubbles` only exists on the v1 API, so the whole stack stays on v1.
- `sashabaranov/go-openai` (model client), `BurntSushi/toml` (config).

## Conventions & gotchas

- **Bubble Tea copies the Model on every Update.** Mutable state you write to
  must be copy-safe: keep `strings.Builder` fields as `*strings.Builder`
  initialized in `New`. Writing to a copied non-zero builder panics.
- **The composer textarea must be `.Focus()`ed at construction** (in `New`,
  not `Init`, which runs on a value copy) — an unfocused textarea silently
  drops keystrokes.
- **Flush the stream buffer before finishing a stream** (`flushStream()` then
  `finishStreaming()`) or the final deltas are never persisted.
- Config errors are typed (`*config.NotFoundError`, `*config.InvalidConfigError`
  with the `config.ErrNoProviders` sentinel) and mapped to friendly messages in
  `cmd/garess/main.go` — preserve that pattern.
- Config search order: `~/.config/garess/config.toml` → `.garess/config.toml` →
  `config.toml` (project root fallback). Endpoints must include `/v1`.
- API keys: `GA_RESS_API_KEY` env var wins over the `api_key` config field.
- Memory notes: local `.garess/memory/`, global `~/.config/garess/memory/`.
- `tui.New` takes `workDir string` (used for AGENTS.md discovery).
- The runner owns session persistence — the TUI never writes to the session
  service directly; it pumps ADK `session.Event`s and renders them.
- Tool calls use **native OpenAI function calling** (no `<tool_call>` text
  protocol). The allow/ask/deny policy is enforced via
  `tools.DenyCallback` (deny) and per-tool `RequireConfirmationProvider` (ask →
  ADK HITL confirmation round trip, answered with `y`/`n` in the TUI).

## Testing

- TUI tests drive Bubble Tea deterministically with
  `tea.WithInput(blockingReader{})` + `tea.WithOutput(io.Discard)`, injecting
  keys via `prog.Send(tea.KeyMsg{...})`. `prog.Run()` returns the final model
  as a **value** `Model` (not a pointer).
- The LLM client is tested against an `httptest` fake OpenAI endpoint — never
  hit real networks in tests.
- `go test ./...` must pass before finishing.

## Roadmap

- Phase 2 — implemented on the ADK runner: built-in functiontools for bash,
  read/write file, glob, grep, and memory operations, gated by regex
  allow/deny/ask approvals with precedence `deny > ask > allow` (deny via a
  `BeforeToolCallback`, ask via ADK's HITL confirmation round trip). The agent
  loop (model → tool call → execute → feed back → repeat) runs inside the ADK
  runner; the TUI renders its events. The custom chat-completions `model.LLM`
  in `internal/llm` keeps llama.cpp compatibility and `reasoning_content`
  thinking (hidden behind `ctrl+t`).
- Phase 3 — git-like hooks (session/tool/model events; exit-code abort) via
  ADK `plugin.Plugin` (runner.Config.PluginConfig).
- Phase 4 — Landlock sandboxing for tools (pluggable backend, `none` fallback;
  RPi bullseye kernels lack Landlock).
- Phase 5 — on-device validation on the Raspberry Pi 1.
  /h
