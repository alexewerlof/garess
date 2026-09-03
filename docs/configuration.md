# Configuration

`garess` is configured with TOML. The shipped
[`example-config.toml`](../example-config.toml) is a complete, commented
reference — copy it and edit.

## Discovery and merge

Config is discovered in this order and overlaid (later files win):

1. **Global**: `~/.config/garess/config.toml` (or
   `$XDG_CONFIG_HOME/garess/config.toml`)
2. **Project**: `.garess/config.toml` in the working directory
3. **Project root**: `config.toml` in the working directory (used if the
   `.garess/` file does not exist)

`garess --config <path>` uses exactly that file. Providers merge by name
(project overrides global for the same provider name); other sections are
overridden wholesale, with per-event and per-list semantics documented below.

## Environment variables

| Variable                                                     | Effect                                                                  |
| ------------------------------------------------------------ | ----------------------------------------------------------------------- |
| `GA_RESS_API_KEY`                                            | API key; wins over the `api_key` config field                           |
| `GARESS_TOOL_ALLOW` / `GARESS_TOOL_ASK` / `GARESS_TOOL_DENY` | tool approval regexes (`;`-separated) — see [Safety model](safety.md)   |
| `GARESS_SANDBOX`                                             | override the `[sandbox] backend` for one run (`none`/`auto`/`landlock`) |

## Top level

```toml
default_provider = "llamacpp"   # which [[providers]] entry is active by default
```

## Providers

```toml
[[providers]]
name = "llamacpp"                          # unique name; used by /model and --provider
endpoint = "http://127.0.0.1:8080/v1"      # must include the /v1 prefix
api_key = ""                               # or GA_RESS_API_KEY
model = "qwen2.5-1.5b-instruct"            # model served by that endpoint
```

Multiple `[[providers]]` blocks are allowed. At launch, `--provider <name>`
selects one and `--model <name>` overrides its model; at runtime `/model`
does the same.

## `[tui]`

```toml
[tui]
theme = "dark"   # auto | dark | light
```

## `[session]`

```toml
[session]
history_limit = 40   # conversation messages kept in the model context
```

Older events stay in the transcript on disk; only the most recent
`history_limit` messages are sent to the model.

## Hooks

Git-style shell hooks fire on agent events. Each hook is a `[[hooks]]` entry;
a hook list for an event in a project config **replaces** the global list for
that event.

```toml
[[hooks]]
event = "before_tool"          # blocking: non-zero exit aborts the tool
command = "/path/to/guard.sh"
timeout = "5s"                 # optional; default 10s

[[hooks]]
event = "after_model"          # notification: logging / usage accounting
command = "echo model done >> /tmp/garess-hooks.log"
```

### Invocation contract

A hook runs as `sh -c <command> garess-hook <event>`, so:

- `$1` (argv[1]) is the event name, also available as `$GARESS_HOOK_EVENT`.
- A JSON payload describing the event is piped to the hook's stdin.
- stdout/stderr are captured — they never touch the TUI — and appear in logs
  and blocking errors.
- The hook is killed (whole process group) if it exceeds `timeout`.

Example payload for `before_tool`:

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

Payload fields are truncated to a few KB so a hook can never be flooded by a
file body or a full reply.

### Events

Blocking events abort the operation when the hook exits non-zero:

`before_run`, `on_user_message`, `before_agent`, `before_model`, `before_tool`

Notification events never abort; failures are logged as warnings:

`after_run`, `after_agent`, `after_model`, `on_model_error`, `after_tool`,
`on_tool_error`, `on_event`

`after_model` and `on_event` fire only on completed (non-partial) responses —
once per model generation, never once per streamed token. A blocked
`before_tool` is fed back to the model as a tool error so the agent can
adapt. `garess doctor` lists your configured hooks.

## Sandbox

```toml
[sandbox]
backend = "auto"                  # none (default) | auto | landlock
write_dirs = ["/home/you/.cache"] # extra writable dirs (build caches, ...)
```

`backend`:

- `none` — no sandboxing (the default).
- `auto` — use Landlock when the kernel supports it, else fall back to `none`.
- `landlock` — require Landlock; garess refuses to start if it cannot apply.

`GARESS_SANDBOX=none|auto|landlock` overrides the backend for a single run.

When active, the whole process may only **write, create, remove or truncate**
files under the writable directories; reads and execution stay unrestricted
everywhere. Writable by default: the project workdir (covers `.garess/`
state), `/tmp`, and the global `~/.config/garess` directory. Add build caches
or other locations with `write_dirs` (absolute paths; project config replaces
the global list).

Enforcement uses `landlock_restrict_self(TSYNC)` and needs **Landlock ABI ≥ 6**
(kernel ≥ 6.7). Kernels without Landlock — including every Raspberry Pi OS
armv6 `rpi-v6` kernel — fall back to `none`, so on a Pi 1 the sandbox always
runs in `none` mode. `garess doctor` reports the kernel ABI, backend, and
writable directories.

See [Safety model](safety.md) for what the sandbox does and does not protect.
