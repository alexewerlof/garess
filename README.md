# garess — a minimal AI harness

A minimal, fast AI harness written in Go. It talks to any OpenAI-compatible
endpoint (local [llama.cpp](https://github.com/ggerganov/llama.cpp), DeepSeek,
OpenRouter, …) through a Claude-Code-style TUI, with project + global memory
and resumable session transcripts.

Current milestone: **chat-first MVP** (config, streaming TUI, memory). The
agentic tool loop, approvals, hooks and Landlock sandboxing are designed for
later phases.

## Features (MVP)

- OpenAI-compatible model endpoint, configured in TOML; works with llama.cpp,
  DeepSeek, OpenRouter, etc.
- Multiple named providers, switchable at runtime with `/model`.
- Claude-Code-style TUI: streaming markdown, multi-line composer, status bar.
- Memory notes in two scopes: project (`.garess/memory/`) and global
  (`~/.config/garess/memory/`).
- [AGENTS.md](https://agents.md) + pi.dev-style `SYSTEM.md` support: project
  and user instruction files are injected into the model context; `@import`
  and `{{VAR}}` are expanded.
- Session transcripts as JSONL in `.garess/sessions/`.
- `garess doctor` to validate config, connectivity and sandbox support.
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
internal/tui/        Bubble Tea UI (bubbletea v2 + lipgloss + glamour)
```

## Roadmap

- Phase 2 — agentic tool loop (bash, file read/write, glob, grep, memory tools)
  with regex allow/deny/ask approvals.
- Phase 3 — git-like hooks (session/tool/model events).
- Phase 4 — Landlock sandboxing for tool execution (pluggable, `none` fallback).
- Phase 5 — on-device validation on the Raspberry Pi 1.
