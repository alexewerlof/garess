# Using the TUI

`garess` is a chat application with a Claude-Code-style terminal UI. The
model streams markdown into the conversation view; you type into a composer
at the bottom. This page covers day-to-day use. Configuration lives in
[Configuration](configuration.md), and what the agent is allowed to do is
covered in the [Safety model](safety.md).

## Keys

| Key                                | Effect                                                                                           |
| ---------------------------------- | ------------------------------------------------------------------------------------------------ |
| `enter`                            | send the prompt                                                                                  |
| `ctrl+j`                           | insert a newline in the composer                                                                 |
| `up` / `down`                      | scroll the conversation when it overflows; otherwise move the cursor                             |
| `pgup` / `pgdown` / `home` / `end` | scroll the conversation                                                                          |
| `ctrl+t`                           | show / hide the model's thinking (reasoning). Thinking is captured and stored, hidden by default |
| `esc`                              | stop the current response (or deny a pending confirmation)                                       |
| `ctrl+c`                           | quit                                                                                             |
| `y` / `n`                          | approve / deny a tool call that asks for confirmation                                            |

## Slash commands

Type any of these at the composer and press `enter`. `/help` shows them in the
app.

| Command                           | Effect                                                                                   |
| --------------------------------- | ---------------------------------------------------------------------------------------- |
| `/help`                           | show the command reference                                                               |
| `/new`                            | start a fresh session (previous transcript stays on disk)                                |
| `/quit`                           | exit                                                                                     |
| `/model <name>`                   | switch provider: `/model deepseek` or `/model deepseek/deepseek-chat` (provider + model) |
| `/notes list`                     | list memory notes                                                                        |
| `/notes read <name>`              | show a note (a local note shadows a global one of the same name)                         |
| `/notes write [-g] <name> <text>` | save a note (`-g` writes to the global scope)                                            |
| `/notes rm <name>`                | delete a local note                                                                      |
| `/agents`                         | show which AGENTS.md / SYSTEM.md files are in effect                                     |
| `/agents reload`                  | re-read AGENTS.md / SYSTEM.md from disk                                                  |
| `/skills`                         | show installed skills                                                                    |
| `/skills reload`                  | re-read skills from disk                                                                 |
| `/tools`                          | show the current tool policy and the built-in tools                                      |
| `/compress [instructions]`        | compress the conversation into a summary (`/compact` works too)                          |

## Context usage and compression

The status bar (bottom line) always shows a `ctx` readout with the context
usage of the current session — for example `ctx ≈12% · 30k/262k used · 232k
free` (≈ means the value is an estimate or the window is a fallback guess; set
the provider's `context_window` for accuracy). After the first reply you know
exactly how much room is left: garess asks the server for real token usage on
every response (`stream_options.include_usage`, supported by llama.cpp and
most OpenAI-compatible servers) and shows those exact numbers. If a server
cannot report usage, garess estimates it locally (≈1 token per 4 characters
over the conversation + instructions) and marks it with `≈`.

When usage reaches the auto-compress threshold (default 80% of the window —
see [`configuration.md`](configuration.md)), garess compresses automatically
after the turn: an in-conversation indicator in the history area (above the
input box) shows `auto-compressing context…`, then a card reports the result
(`Context: 1,024 → 337 tokens (freed 687) · usage 0.4% → 0.1% of 262k`).
Auto-compression runs after every completed turn and, as a backstop, before
a message you send while the context is already over the threshold.

`/compress [instructions]` (or `/compact`) compresses on demand (the status
shows `compressing context…`); the optional instructions tell the summarizer
what to preserve (e.g. `/compress keep the API design decisions`). Older
exchanges become one compact digest and the most recent exchange stays
verbatim — what you see is exactly what the model sees next. The full JSONL
transcript is preserved on disk; only the model's view is compacted.

## Memory notes

Notes are plain markdown files the model can read and write with the
`memory_*` tools, and you can manage with `/notes`. There are two scopes:

- **Local / project**: `.garess/memory/` — tied to the directory you launch
  garess from.
- **Global / user**: `~/.config/garess/memory/` — applies everywhere.

A note name like `user` becomes `user.md`. When reading, a local note shadows
a global note with the same name; `/notes list` shows both with their scope.

Notes are a lightweight substitute for agent memory: tell the agent to save
project conventions or decisions, and it will read them back in later
sessions. Names are validated to prevent path traversal (`/` and `\` are
rejected).

## Sessions and transcripts

Every launch of `garess` starts a new session. Completed conversations are
persisted as JSONL transcripts in `.garess/sessions/`:

- `<id>.jsonl` — one JSON-encoded session event per line
- `<id>.meta.json` — session metadata (app name, user id, state)

`/new` starts another session within the same process; older transcripts are
left on disk for reference. Streamed partials are not persisted — only
completed events — so a transcript is a clean record of what actually
happened, including tool calls and results.

## Steering the model: AGENTS.md, SYSTEM.md, skills

`garess` injects instruction files into the model context on every turn. This
is how you give the agent durable behavior without repeating yourself.

- **Project** `AGENTS.md` and `SYSTEM.md` in the directory where garess was
  launched (the search stops there — parent directories are not walked).
- **User** files, applied everywhere: `$XDG_CONFIG_HOME/agents/AGENTS.md`,
  `~/.agents/AGENTS.md`, and `~/.config/garess/SYSTEM.md` (a pi.dev-style
  system prompt).

Supported syntax in these files:

- `@path/to/file` on its own line imports another file (relative to the
  current file; globs allowed; cycles and missing files are errors).
- `{{VAR}}` placeholders are expanded from the environment.

Skills are the same idea, packaged per-topic. They are discovered from
`~/.config/garess/skills/<name>/SKILL.md` (user) and
`.garess/skills/<name>/SKILL.md` (project), or the repo-local
`<launchdir>/skills/<name>/SKILL.md`. A skill directory may also use
`README.md` instead of `SKILL.md`. Skills support the same `@import` and
`{{VAR}}` expansion.

`/agents`, `/agents reload`, `/skills`, and `/skills reload` let you inspect
and refresh these without restarting.

## Multiple providers

Define several providers in the config (see
[Configuration](configuration.md)) and switch between them:

```sh
garess --provider deepseek                       # at launch
/model deepseek/deepseek-chat                    # at runtime (provider/model)
```

`garess doctor` is the fastest way to check a provider's endpoint before you
start chatting. It also lists the configured hooks, MCP servers (with the
tools each exposes) and the AGENTS.md/SYSTEM.md files that apply.
