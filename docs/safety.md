# Safety model

`garess` is an **agentic coding harness**: the model can run shell commands
and read/write files on your machine. This page is the honest summary of the
controls that exist, their defaults, and their limits. Read it before letting
the agent loose on anything you care about.

There are three layers, applied in order:

1. **Tool approval policy** (allow / ask / deny) — decides whether a tool call
   runs at all.
2. **Hooks** — shell gates on events (e.g. block before a tool runs).
3. **Landlock sandbox** — kernel write confinement, opt-in.

## Default posture: tools are allowed

Unless you configure a policy, **every built-in tool is allowed with no
confirmation**. That is the default on purpose (a friction-free coding
assistant), but it means the model can, in principle, run arbitrary `bash`
and write anywhere the OS user can. The `ask`/`deny` machinery exists for
when you care.

The only hard caps that always apply: `bash` commands run with a 15-second
timeout, and file reads/writes are done by the tool handlers under your user
account.

## 1. Tool approval policy

The policy is a set of Go regular expressions read from environment
variables at startup:

| Variable            | Meaning                                                   |
| ------------------- | --------------------------------------------------------- |
| `GARESS_TOOL_DENY`  | calls matching these regexes are blocked                  |
| `GARESS_TOOL_ASK`   | calls matching these regexes request a `y`/`n` in the TUI |
| `GARESS_TOOL_ALLOW` | calls matching these regexes run without confirmation     |

Precedence is **deny > ask > allow**. Two more rules matter:

- An **empty policy** (no variables set) allows everything.
- A **non-empty policy fails closed**: if no rule matches a call, the call is
  **denied**.

Regexes match a target string built from the call:
`<tool-name> <key>=<value> <key>=<value>` (keys sorted). Examples:

```sh
# Block destructive rm -rf through the bash tool
export GARESS_TOOL_DENY='bash command=.*rm -rf'

# Ask before any bash call that mentions curl or wget
export GARESS_TOOL_ASK='bash command=.*(curl|wget).*'
```

Because deny > ask > allow, an `allow` rule cannot override a `deny`; `allow`
only affects calls no `deny`/`ask` rule matched, and every call in a
non-empty policy that matches nothing is denied anyway.

How each decision is enforced:

- **deny** — the tool is blocked before it runs; the model receives a tool
  error ("denied by policy") and can adapt.
- **ask** — garess pauses the run and shows `Approve bash(command=…)?`; you
  answer `y`/`n` in the TUI. The tool only runs if you approve.
- **allow** — the tool runs immediately.

`/tools` in the TUI shows the effective policy and the built-in tools.
Built-ins: `bash`, `read_file`, `write_file`, `glob`, `grep`,
`memory_read`, `memory_write`, `memory_list`, `memory_delete`.

MCP servers (see [Configuration](configuration.md)) expose external tools
that are treated like any other tool: **deny/ask/allow rules match MCP tool
calls too**, using the same `<name> <key>=<value> …` target. There is no
separate sandbox for them — a configured MCP server runs its own code
remotely, so treat it as trusted. An unreachable MCP server is skipped for
that turn rather than failing the run.

## 2. Hooks as gates

Hooks are shell commands you configure (see
[Configuration](configuration.md#hooks)). The blocking ones are pre-event
gates: `before_run`, `on_user_message`, `before_agent`, `before_model`,
`before_tool`. If a blocking hook exits non-zero, the operation is aborted —
for `before_tool` the model sees the failure as a tool error. Hooks receive a
JSON payload on stdin and can therefore implement guardrails such as "no
writes outside the project directory" or "no network calls to internal
hosts". Hooks are as trustworthy as the machine they run on and the commands
you configure; they are a policy layer, not a sandbox.

## 3. Landlock write sandbox

The sandbox (see [Configuration](configuration.md#sandbox)) uses the Linux
Landlock LSM to confine **writes** of the whole process — the in-process file
tools _and_ every `bash` child:

- **Confined:** creating, writing, truncating, removing files and dirs,
  creating sockets/fifos/symlinks — allowed only under the writable
  directories (project workdir, `/tmp`, `~/.config/garess`, plus your
  `write_dirs`).
- **Not confined:** reads and command execution anywhere. A coding agent must
  inspect system files and run programs; the sandbox deliberately does not
  restrict those.

Limits to know:

- It is **write confinement**, not a full container. A malicious model can
  still read anything your user can read and execute anything your user can
  execute; the sandbox only stops it from _modifying_ files outside the
  allowlist.
- It needs **Landlock ABI ≥ 6** (Linux ≥ 6.7). Desktop distros from ~2024
  qualify. Raspberry Pi OS armv6 (`rpi-v6`) kernels have Landlock disabled
  entirely, so on a Pi 1 the sandbox always falls back to `none`.
- `backend = "landlock"` **fails closed**: garess refuses to start if it
  cannot apply. `backend = "auto"` degrades to `none` with a logged warning.
- The sandbox is whole-process and irreversible once applied — garess applies
  it last, after creating every directory it needs.

## Recommendations

- Start with a **non-empty policy** (default-deny) and `ask` for anything
  mutating, e.g. `GARESS_TOOL_ASK` for `write_file` and broad `bash`.
- Prefer a **local model** (llama.cpp) or a provider you trust: the model is
  the primary actor; the controls above only bound what it may do.
- Keep your config private (`chmod 600`); it can contain API keys.
- Remember the tool approval policy applies to **tool calls**, and the
  sandbox to **writes**. Neither stops the model from _reading_ files, and
  both are irrelevant to plain (non-tool) chat.
- Run garess as an unprivileged user, ideally in a directory you are happy
  for it to modify.
