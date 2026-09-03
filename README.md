# garess — a minimal AI harness

A minimal, fast AI harness written in Go. It talks to any OpenAI-compatible
endpoint (local [llama.cpp](https://github.com/ggerganov/llama.cpp), DeepSeek,
OpenRouter, …) through a Claude-Code-style TUI, with project + global memory,
resumable session transcripts and git-style shell hooks.

Current milestone: **all phases implemented and Pi-validated** — the ADK
runner drives the tool loop, approvals (allow/ask/deny), HITL confirmations,
shell hooks and a Landlock tool sandbox, all validated on-device on the
Raspberry Pi 1.

## Features

- OpenAI-compatible model endpoint, configured in TOML; works with llama.cpp,
  DeepSeek, OpenRouter, etc.
- Multiple named providers, switchable at runtime with `/model`.
- Claude-Code-style TUI: streaming markdown, multi-line composer, status bar.
- Agentic tool loop: `bash`, `read_file`, `write_file`, `glob`, `grep` and
  memory tools, gated by a regex allow/ask/deny policy (deny > ask > allow).
  `ask` tools request human confirmation in the TUI before running.
- Git-style shell hooks on agent/tool/model/session events, with exit-code
  abort on blocking events (see [Hooks](#hooks)).
- Opt-in Landlock sandbox (Phase 4) confining tool **writes** to the project
  while leaving reads and execution unrestricted (see
  [Sandbox](#sandbox)).
- Memory notes in two scopes: project (`.garess/memory/`) and global
  (`~/.config/garess/memory/`).
- [AGENTS.md](https://agents.md) + pi.dev-style `SYSTEM.md` support: project
  and user instruction files are injected into the model context; `@import`
  and `{{VAR}}` are expanded.
- Session transcripts as JSONL in `.garess/sessions/`.
- `garess doctor` to validate config, connectivity, hooks and sandbox support.
- Cross-compiles to a static 32-bit ARMv6 binary for the Raspberry Pi 1.

## Install & build

Requires Go ≥ 1.24 (pure-Go dependencies, no cgo).

```sh
make build          # host binary -> dist/garess
make test           # run the test suite
make vet
make build-arm      # RPi 1: static linux/arm GOARM=6 -> dist/garess-linux-armv6
```

Copy the ARMv6 binary to the Pi and run it there (no Go needed on the Pi).

## Configuration

Config is TOML, discovered in this order and merged (project overrides global):

1. **Global**: `~/.config/garess/config.toml` (or `$XDG_CONFIG_HOME/garess/config.toml`)
2. **Project**: `.garess/config.toml` in the working directory
3. **Project root**: `config.toml` in the working directory (fallback if the
   `.garess/` one does not exist)

You can also point at a specific file with `--config <path>`. Start from
[`example-config.toml`](example-config.toml):

```sh
mkdir -p ~/.config/garess
cp example-config.toml ~/.config/garess/config.toml
$EDITOR ~/.config/garess/config.toml
```

The API key may be omitted from the file and provided via the
`GA_RESS_API_KEY` environment variable (it takes precedence). Keep the config
file private: `chmod 600 ~/.config/garess/config.toml`.

## Hooks

Git-style shell hooks fire on agent events and can gate what the agent does.
Each hook is a `[[hooks]]` entry in the config (project config replaces global
hooks for the same event):

```toml
# Block risky tool calls: non-zero exit aborts the tool.
[[hooks]]
event = "before_tool"
command = "/path/to/guard.sh"
timeout = "5s"          # optional; default 10s

# Audit every completed session event (notification only).
[[hooks]]
event = "on_event"
command = "jq '{event, agent, tool}' >> /tmp/garess-events.jsonl"
```

A hook runs with `sh -c <command>`; the event name is `$1` (argv[1]) and the
`GARESS_HOOK_EVENT` env var, and a JSON payload describing the event is piped
to stdin:

```json
{
  "event": "before_tool",
  "app_name": "garess",
  "user_id": "local",
  "session_id": "…",
  "invocation_id": "…",
  "agent": "garess",
  "tool": "bash",
  "args": { "command": "rm -rf /" }
}
```

Payloads are truncated (a few KB max) so hooks can never be flooded by a file
body or full reply; stdout/stderr are captured and never touch the TUI.

**Events.** Blocking ("pre") events abort the operation on a non-zero exit:
`before_run`, `on_user_message`, `before_agent`, `before_model`,
`before_tool`. Notification events never abort; a failure is logged:
`after_run`, `after_agent`, `after_model`, `on_model_error`, `after_tool`,
`on_tool_error`, `on_event`. `after_model` and `on_event` fire only on
completed (non-partial) responses — once per model generation, never once
per streamed token.
A blocked `before_tool` is fed back to the model as a tool error, so the agent
can adapt. `garess doctor` lists your configured hooks.

## Sandbox

Phase 4 adds an opt-in, kernel-enforced **write sandbox** on top of the
allow/ask/deny policy. When enabled, the whole garess process (the in-process
file tools _and_ every `bash` child) may only **write, create, remove or
truncate** files under the writable directories; reads and command execution
stay unrestricted everywhere, so the agent can still inspect system files.

```toml
[sandbox]
backend = "auto"                  # none (default) | auto | landlock
write_dirs = ["/home/you/.cache"] # extra writable dirs (build caches, ...)
```

- `none` — no sandboxing (the default).
- `auto` — Landlock when the kernel supports it, else fall back to `none`.
- `landlock` — require Landlock; garess refuses to start if it cannot apply.
- `GARESS_SANDBOX=none|auto|landlock` overrides the backend for a run.

Writable by default: the project workdir (which covers `.garess/` state),
`/tmp`, and the global `~/.config/garess` directory. Enforcement uses
`landlock_restrict_self(TSYNC)`, which needs **Landlock ABI ≥ 6** (kernel
≥ 6.7) to confine all threads of a multi-threaded Go process. Kernels without
Landlock fall back to `none` — including every Raspberry Pi OS armv6 `rpi-v6`
kernel (verified on 6.18.34+rpt-rpi-v6: `landlock_create_ruleset` returns
ENOSYS), so on the RPi 1 the sandbox always runs in `none` fallback mode.
`garess doctor` reports the kernel ABI and your backend/dirs.

## AGENTS.md & SYSTEM.md

[AGENTS.md](https://agents.md) files tell the model how to behave in a given
project. garess also supports a lightweight pi.dev-style `SYSTEM.md` file for
the same purpose. Both are discovered at startup and injected into every
request as a leading system message:

- **User** (apply everywhere): `$XDG_CONFIG_HOME/agents/AGENTS.md` (or
  `~/.config/agents/AGENTS.md`), then `~/.agents/AGENTS.md`; plus
  `~/.config/garess/SYSTEM.md` (like pi.dev's `~/.pi/agent/SYSTEM.md`).
- **Project**: the `AGENTS.md` and `SYSTEM.md` in the directory where garess
  was launched. The search is capped there — parent directories are not
  walked.

Supported syntax: `@path/to/file` imports (relative to the file, globs
allowed) and `{{VAR}}` placeholders expanded from the environment.

```sh
/agents           # show which files are in effect
/agents reload    # re-read AGENTS.md / SYSTEM.md from disk
```

`garess doctor` also lists the AGENTS.md / SYSTEM.md files that apply.

## Skills

Skills are optional instruction packs that are discovered and injected into the
chat context the same way AGENTS.md files are. They live in directories named
like:

- `~/.config/garess/skills/<skill>/SKILL.md` (user-wide)
- `.garess/skills/<skill>/SKILL.md` (project-local)

A skill directory may also use `README.md` in place of `SKILL.md`. Skills accept
`@import` and `{{VAR}}` expansion, and they can be inspected with `/skills` and
`/skills reload`.

## Usage

```sh
garess                  # start the chat TUI
garess doctor           # check config, endpoint, Landlock support
garess --provider deepseek --model deepseek-chat
```

TUI keys and commands — type `/help` inside the app:

| Command                           | Effect                                            |
| --------------------------------- | ------------------------------------------------- |
| `/model <name>`                   | switch provider (`/model deepseek/deepseek-chat`) |
| `/notes list`                     | list memory notes                                 |
| `/notes read <name>`              | show a note (local shadows global)                |
| `/notes write [-g] <name> <text>` | save a note (`-g` = global)                       |
| `/notes rm <name>`                | delete a local note                               |
| `/new`                            | start a fresh session                             |
| `/quit`                           | exit                                              |

Keys: `enter` sends, `ctrl+j` inserts a newline, `esc` stops the current
response, `ctrl+c` quits.

## Layout

```
cmd/garess/          entry point, flags, doctor
internal/config/     TOML config, XDG paths, merge, validation
internal/llm/        OpenAI-compatible client (sashabaranov/go-openai)
internal/chat/       sessions + JSONL transcripts
internal/memory/     local + global notes
internal/hooks/      git-style shell hooks (config [[hooks]], ADK plugin)
internal/sandbox/    Landlock write sandbox (none/auto/landlock backends)
internal/harness/    ADK agent + runner wiring (tools, policy, hooks)
internal/tui/        Bubble Tea UI (bubbletea + lipgloss + glamour)
```

## Roadmap

- Phase 2 — agentic tool loop (bash, file read/write, glob, grep, memory tools)
  with regex allow/deny/ask approvals. ✅
- Phase 3 — git-style hooks (session/tool/model events). ✅
- Phase 4 — Landlock sandboxing for tool execution (pluggable, `none` fallback). ✅
- Phase 5 — on-device validation on the Raspberry Pi 1. ✅ (2026-09-03: all
  test suites run green cross-compiled on the Pi; a real agentic session
  against the LAN llama.cpp completed; three fixes landed — sandbox
  fail-closed, hook timeout process-group kill, `after_model` per-generation).
