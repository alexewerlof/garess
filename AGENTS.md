# AGENTS.md

garess is a minimal AI harness written in Go: an OpenAI-compatible chat client
with a Claude-Code-style TUI, project/global memory, JSONL session transcripts
and AGENTS.md/SYSTEM.md support. Model "thinking" (reasoning) is captured and stored but
hidden by default — press `ctrl+t` (as in Pi) to show or hide it. It
cross-compiles to a static 32-bit ARMv6 binary for a Raspberry Pi 1.

This file is read by garess itself: running `garess` from this directory
injects it into the model context as a system message (see
`internal/agents/`). The project search is capped at the launch directory.

Human-facing documentation lives in [`docs/`](docs/) (end-user and
developer guides). This file and the per-package `AGENTS.md` files are the
AI-agent navigation layer — each package's `AGENTS.md` is authoritative for
its folder.

## Commands

```sh
make build       # host binary -> dist/garess
make build-arm   # RPi 1 cross-compile -> dist/garess-linux-armv6 (GOARM=6)
make test        # go test ./...
make vet
make fmt         # gofmt -l -w .
make run         # go run ./cmd/garess
make doctor      # config + endpoint + sandbox checks
```

Finish changes with `go build ./... && go test ./...`; keep `gofmt` clean
(`make fmt`). Do not commit build output (`dist/`) or local state
(`.garess/`, `config.toml` at root — gitignored).

## Layout

| Path                | Purpose                                                                               |
| ------------------- | ------------------------------------------------------------------------------------- |
| `cmd/garess/`       | Entry point, flags, `doctor` subcommand, friendly config errors                       |
| `internal/agents/`  | AGENTS.md + SYSTEM.md discovery + rendering (`@import` imports, env-var substitution) |
| `internal/chat/`    | Sessions + JSONL transcripts (`.garess/sessions/`)                                    |
| `internal/config/`  | TOML config, XDG paths, merge, typed errors                                           |
| `internal/harness/` | ADK agent + runner wiring (tools, policy, hooks plugin)                               |
| `internal/hooks/`   | Git-style shell hooks (config `[[hooks]]`, ADK plugin, exit-code abort)               |
| `internal/llm/`     | Custom ADK `model.LLM` over OpenAI-compatible chat completions (no SDK)               |
| `internal/memory/`  | Local + global memory notes                                                           |
| `internal/sandbox/` | Landlock tool sandbox (Phase 4): write confinement, none/auto/landlock backends       |
| `internal/skills/`  | Skill pack discovery + rendering (user/project/launch-dir roots)                      |
| `internal/tui/`     | Bubble Tea UI (model, streaming, markdown, commands)                                  |

Each package has its own `AGENTS.md` with its specific conventions — read the
one in the folder you are editing.

## Stack

- Go module `garess`; pure-Go dependencies only (`CGO_ENABLED=0` for the RPi
  build).
- **Charm stack is v1**: bubbletea v1.3.10, bubbles v1.0.0, lipgloss v1.1.0,
  glamour v1.0.0 (`github.com/charmbracelet/*` paths). Do NOT "upgrade" to v2 —
  `bubbles` only exists on the v1 API, so the whole stack stays on v1.
- `google.golang.org/adk/v2` v2.2.0 (agent runner, tools, plugins, session
  types) + `google.golang.org/genai` (content types). The chat-completions
  wire format is hand-rolled in `internal/llm` — no OpenAI SDK dependency.
- `BurntSushi/toml` (config).

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
- The Phase 4 sandbox is **write confinement**, not full containment: reads
  and execution stay unrestricted; only writes/creates/removes are confined
  to the writable dirs. It is opt-in (`[sandbox] backend = none|auto|landlock`,
  `GARESS_SANDBOX` env override), whole-process and irreversible, applied last
  in `runTUI`. Needs Landlock ABI >= 6 (kernel >= 6.7) for process-wide TSYNC.
  On the RPi 1 the sandbox always falls back to `none`: Raspbian's armv6
  `rpi-v6` kernel ships with Landlock disabled entirely — verified ENOSYS on
  6.18.34+rpt-rpi-v6 — so only the fallback path runs on-device.

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
- Phase 3 — implemented via ADK `plugin.Plugin` on `runner.Config.PluginConfig`
  (`internal/hooks`): git-style shell hooks from `[[hooks]]` config entries
  covering session (before/after run, on*event, on_user_message), agent,
  tool and model events. Hooks run via `sh -c` with the event name as `$1`
  and a JSON payload on stdin; `before*\*`hooks abort on non-zero exit.
Config validation of event names/timeouts happens in`hooks.Parse`;
`garess doctor`lists configured hooks.`after_model`and`on_event`fire only
on non-partial (completed) responses — once per model generation, never per
streamed token. Hook timeouts kill the whole`sh`process group (Setpgid +`cmd.Cancel` group kill), so an orphaned child cannot outlive the timeout.
- Phase 4 — implemented: Landlock sandboxing for tools (`internal/sandbox`,
  `[sandbox]` config, pluggable `none`/`auto`/`landlock` backends). Whole-
  process write confinement via `landlock_restrict_self(TSYNC)` (ABI >= 6);
  reads/exec unrestricted. Explicit `landlock` fails closed with an Err when
  the kernel has no Landlock (ABI <= 0) or ABI < 6; `auto` degrades to `none`.
  Raw x/sys/unix syscalls, no go-landlock
  (its libcap/psx dep needs cgo, breaking the armv6 build).
- Phase 5 — on-device validation on the Raspberry Pi 1 (kernel 6.18 rpi-v6,
  armv6l). ✅ Doctor/connect/hooks/sandbox verified over SSH; the full test
  suites for sandbox, hooks and harness run green cross-compiled on the Pi
  (real `sh`, fake model). A real agentic session against the LAN llama.cpp
  completed in ~45s: bash + write_file tools ran, hooks fired with payloads,
  and the JSONL session persisted. Findings fixed during validation: (1)
  explicit `landlock` silently ran unsandboxed on Landlock-less kernels
  (fail-closed bug, now Err) — and Raspbian's rpi-v6 kernel has no Landlock
  even at 6.18; (2) hook timeouts only killed `sh`, not its children (dash
  forks), so a timed-out hook blocked the caller — now a process-group kill +
  WaitDelay; (3) `after_model` fired once per streamed token (134 shell
  spawns per turn) — now once per model generation like `before_model`.
