# AGENTS.md

garess is a minimal AI harness written in Go: an OpenAI-compatible chat client
with a Claude-Code-style TUI, project/global memory, JSONL session transcripts
and AGENTS.md/SYSTEM.md support. Model "thinking" (reasoning) is captured and
stored; while the model thinks, a live `Thinking` row appears in the
conversation under your message — press `ctrl+t` (as in Pi) to expand it to
the reasoning text, or hide it again. It
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
| `cmd/garess/`       | Entry point, flags, subcommands (`init`, `doctor`, `help`), friendly config errors    |
| `internal/agents/`  | AGENTS.md + SYSTEM.md discovery + rendering (`@import` imports, env-var substitution) |
| `internal/chat/`    | Sessions + JSONL transcripts (`.garess/sessions/`)                                    |
| `internal/config/`  | TOML config, XDG paths, merge, typed errors, embedded example template (`Example()`)  |
| `internal/harness/` | ADK agent + runner wiring (tools, policy, hooks plugin)                               |
| `internal/hooks/`   | Git-style shell hooks (config `[[hooks]]`, ADK plugin, exit-code abort)               |
| `internal/llm/`     | Custom ADK `model.LLM` over OpenAI-compatible chat completions (no SDK)               |
| `internal/mcp/`     | MCP server client: `[[mcp_servers]]` config → ADK toolsets (stdio/sse/http, auth)     |
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
  `garess init` writes a starter config (the bundled example) to
  `./config.toml`, or with `-g` to the global location; `garess -h` lists the
  commands.
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
  thinking (live `Thinking` row in the conversation, reasoning behind `ctrl+t`).
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
- MCP support — `[[mcp_servers]]` config entries (`internal/mcp`) become ADK
  toolsets on the agent: transports `stdio`/`sse`/`http`, optional per-server
  `headers`, `GARESS_MCP_<NAME>_TOKEN` env override (wins over an inline
  Authorization header). Servers are contacted lazily; toolsets are resolved
  per run and an unreachable server DEGRADES to no tools (warning logged,
  `Server.Status`) instead of failing the turn. MCP calls ride the same
  deny/ask policy as built-ins. `garess doctor` lists each server's tools.
  Depends on `github.com/modelcontextprotocol/go-sdk` (pure Go, armv6-safe).
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

## Context compression (Phase 6, 2026-09-04)

- UI shows a live `ctx` usage readout, ALWAYS visible right-aligned on the
  status line (`statusLine` + `contextReadout`), e.g. `ctx ≈12% · 30k/262k
used · 232k free`. Exactness: garess asks for the usage chunk on every
  streamed call via OpenAI `stream_options.include_usage` (internal/llm
  `buildChatRequest`), so `usage.prompt_tokens` is exact after every model
  call on servers that honor it (llama.cpp does). Servers that don't fall
  back to a local estimate (≈1 token / 4 chars over events + preamble,
  `estimatedPromptTokens` run after any turn that saw no usage) — those show
  an `≈`. Window = provider `context_window` config → `/v1/models` probe
  at launch (cmd/garess `resolveContextWindows`) → `DefaultContextWindow`
  (128k, marked `≈`). New config: `providers[].context_window`,
  `session.auto_compress_threshold` (percent, default 80, 0 = off;
  `*int` so 0 can disable — merge copies non-nil pointers).
- Auto-compress runs at TURN BOUNDARIES only: after each completed
  user↔assistant turn (`finishStreaming`) when the estimate is at/over the
  threshold, and as a backstop in `send()` when the context is ALREADY at/over
  the threshold (the pending message itself is not compressible, so its size
  does not trigger). One user message = one `runner.Run` holding
  the whole tool loop; ADK v2.2.0 has no in-loop compaction hook, so there is
  NO mid-loop compression. Giant tool outputs are bounded at the source:
  `bash`/`read_file` results are capped (`tools.MaxToolOutputChars` 32k,
  `capOutput`). Manual `/compress [instructions]` + `/compact` alias.
- Compression is **full-transcript-preserving**: `chat.Service.Compact`
  appends a summary event (author `user`) and records the latest
  `compaction{SummaryEventID, CoveredThroughEventID}` in the `<id>.meta.json`
  sidecar; `Get`/`List` filter the model's view to `[summary] + events after
CoveredThroughEventID` (summary sticky under `history_limit`) — the JSONL
  keeps everything. Each new compaction covers a prefix that includes the
  previous summary, so only the latest marker matters. `internal/compress`
  picks the cutoff (keep the last user exchange verbatim so tool call/result
  pairs never split), flattens the transcript (thinking omitted), and
  summarizes via a sideband non-streaming model call through the provider's
  `LLMModel` (new `harness.Provider.LLMModel` + `.SessionService`).
- The summary renders as a distinct block (not a user bubble) via
  `Model.summaryEventID`; while a compression runs, an in-conversation
  indicator (`compressionIndicator`, rendered in the conversation area above
  the composer — the only compression indicator; the status bar stays plain)
  shows activity — wording differs by `compressIsAuto` (manual says
  "compressing context", auto "auto-compressing context"). On completion an
  ephemeral
  card reports the digest: `Context: X → Y tokens (freed Z) · usage a% → b%
of W` (grouped numbers, 1-decimal % < 10). Keys are gated while
  `compressing` (esc cancels, pending pre-send text is restored on cancel).
  Anti-thrash: `ctxNoAutoAfter` event watermark after each auto-compress.

## TUI look & feel (Phase 7, 2026-09-05)

An opencode-inspired visual redesign (warm-orange identity kept; adapts to
the `tui.theme` dark/light automatically — no new config):

- **Hero empty state.** A new session with no content shows a pixel-block
  `garess` logo (internal/tui/logo.go, 5x5 font, double-width `██`), a
  tagline, the editor panel centered on the terminal background, and a hint
  line; the editor drops to the bottom once a conversation starts.
- **Editor panel.** The composer textarea sits on a subtly lighter background
  spanning the content width (`ui.composer`, `editorBox`) with NO border —
  the background difference is the boundary (opencode-style). Panel height is
  exactly 4 rows (1 top + 1 bottom padding + 2 textarea rows) — keep
  `layout()`'s `composerH` in sync.
- **Message rails.** The conversation view is zone-tagged per line
  (`convView`: `zoneUser`/`zoneAssistant`/`zonePlain`); user messages get a
  blue left rail, assistant messages a rose rail, and everything internal
  (thinking, tool calls/results, compression, summaries, ephemera) is plain
  and dim. Rails are painted as a precomputed per-line ANSI prefix in
  `view()` (O(height), no width math) and skip blank lines so each message
  reads as its own block. `renderEvent` appends one trailing blank line per
  completed event for spacing.
- **Bottom bar.** The old top header is gone; the status line now shows the
  busy state / error / `provider · model` on the left and the `ctx` readout +
  app version on the right. Tool calls/results render as compact dim
  `⚙ name`/`↳ name` blocks (display-capped; the model still sees full output).
- **Slash-command palette.** Typing `/` at the start of the composer lists
  commands from the `commands.go` registry (`name` + description, selected
  row orange). `↑`/`↓` move the selection, `tab`/`enter` complete the
  highlighted command (arguments after the word are kept), `esc` dismisses.
  When the typed word IS a complete command the list hides and `enter` runs
  it (unchanged behavior — `/new`, `/notes x`, etc. still work from tests).
  The registry is the single source of truth for `/help` and the palette.
- **Right rail + resume (implemented 2026-09-05).** `railActive()` is true at
  terminal width >= 140 AND when a session service is wired (cmd passes
  `Options.SessionService` = the chat svc); it reserves `railWidth` = 34 cols
  and draws a full-height column right of the frame (heading, current-session
  card, recent-session list, key hint) via `internal/tui/sessions.go`
  (`railRows`/`railFrame`/`wrapFrame`). Per-frame width math happens ONLY
  while the rail is active (desktop) — at 118 cols (the Pi) it never runs.
  `tab` moves focus to the rail list (`↑↓` pick, `enter` resume, `esc` back);
  `/sessions` opens the same list as an in-conversation picker that works at
  any width. Resuming (`resumeSession`) loads the session's model view
  (`chat.Service.SessionView`: compaction-filtered + history-capped, exactly
  what the runner feeds the model next) into `m.events`/`m.rendered`, sets
  `m.sessionID` (the runner resolves the session per Run — no rebuild), and
  re-estimates ctx usage. The session list is cached on the Model and loaded
  async (`sessionsMsg`/`loadSessions`): at Init, after each finished turn,
  `/new` and resume. `chat.Recent` (newest first, preview = first user line)
  backs the list. See /memories/repo/garess-tui-redesign-notes.md.
- Per-frame rules still apply: rails and panels must not add lipgloss width
  math per visible line; the editor panel's `Width` pass is bounded to its 4
  rows. Validate on the Pi at 118 cols with GARESS_STATS (rail stays off
  there).
