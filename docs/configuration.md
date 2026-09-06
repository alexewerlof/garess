# Configuration

`garess` is configured with TOML. Run `garess init` to write a complete,
commented starter config in the current folder (`garess init -g` writes the
global `~/.config/garess/config.toml` instead) — the example template is
bundled into the binary — then edit it.

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
| `GARESS_MCP_<NAME>_TOKEN`                                    | bearer token for the MCP server `NAME` (wins over an inline header)     |

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
context_window = 32768                      # optional: model context size in tokens
```

Multiple `[[providers]]` blocks are allowed. At launch, `--provider <name>`
selects one and `--model <name>` overrides its model; at runtime `/model`
does the same.

`context_window` sets the model's context size in tokens — the reference for
the `ctx` usage indicator in the header and for the auto-compress threshold.
When it is unset (0), garess probes `GET /v1/models` at launch (best effort)
and falls back to 128k if the endpoint does not advertise a context length.
Endpoints rarely advertise it, so setting `context_window` is recommended
for an accurate indicator.

## `[tui]`

```toml
[tui]
theme = "dark"   # auto | dark | light
```

## `[session]`

```toml
[session]
history_limit = 40              # conversation messages kept in the model context
auto_compress_threshold = 80   # context usage % that triggers auto-compression (0 = off)
```

Older events stay in the transcript on disk; only the most recent
`history_limit` messages are sent to the model.

## Context compression

The status bar always shows a live `ctx` readout — the percentage of the
context window in use, the used/total sizes and the free space (e.g.
`ctx ≈12% · 30k/262k used · 232k free`). The numbers are exact after every
reply because garess asks the server for token usage on each streamed response
(`stream_options.include_usage`); servers that cannot report it fall back to
a local estimate (~1 token per 4 characters over the conversation +
instructions), marked with `≈`. `≈` on the percentage also means the window is
a fallback guess rather than configured.

When the estimated usage reaches `auto_compress_threshold` percent of the
window, garess compresses the conversation **after a completed turn** (tool
outputs can be large, so the check runs right after every turn) and — as a
backstop — **before a prompt sent while the context is already at/over the
threshold**. Manual compression is always available with `/compress
[instructions]` (alias `/compact`), where the optional instructions steer
what the summary should preserve.

Compression summarizes every older exchange up to the most recent user
message into one compact digest and keeps the latest exchange verbatim, so
the model sees `[summary … recent turn]` instead of the full history. The raw
JSONL transcript is never modified: the full conversation stays on disk and
only the model's view is compacted (a marker in the session metadata drives
the read-time filter). While it runs, an in-conversation indicator (in the
history area, above the input box) shows `compressing context…` (manual) or
`auto-compressing context…` (auto); the summary card reports what was freed
(`Context: 1,024 → 337 tokens (freed 687) · usage 0.4% → 0.1% of 262k`). Set
`auto_compress_threshold = 0` to disable automatic compression (manual
`/compress` stays available).

## MCP servers

MCP (Model Context Protocol) servers expose their tools to the agent
alongside the built-ins. Each server is a `[[mcp_servers]]` entry; a project
config replaces the global entry with the same name.

```toml
[[mcp_servers]]
name       = "crawl4ai"              # unique; also the env-token lookup key
url        = "http://crawl-host:11235/mcp/sse"
headers    = { Authorization = "Bearer your-token" }   # optional
```

### Transports

`transport` selects how garess talks to the server (required):

| Transport | Target                  | Use for                                 |
| --------- | ----------------------- | --------------------------------------- |
| `sse`     | `url` (SSE endpoint)    | Docker-hosted servers (e.g. `/mcp/sse`) |
| `http`    | `url` (streamable HTTP) | modern MCP HTTP servers                 |
| `stdio`   | `command` (+ `args`)    | local subprocess servers                |

```toml
[[mcp_servers]]
name = "filesystem"
transport = "stdio"
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
env = { }   # optional extra env for the subprocess
```

### Auth

`sse`/`http` servers get the `headers` map on every request. Set the
`GARESS_MCP_<NAME>_TOKEN` environment variable (NAME uppercased,
non-alphanumeric runes become `_`) to send `Authorization: Bearer <token>` —
it wins over an inline `Authorization` header:

```sh
GARESS_MCP_CRAWL4AI_TOKEN=… garess
```

### Behaviour

- Servers are contacted lazily — startup is never blocked by a configured
  server.
- An **unreachable server is skipped for that turn** (its tools are missing;
  a warning goes to the log) instead of failing the run.
- `garess doctor` lists each configured server and the tools it exposes.
- MCP tool calls ride the same allow/ask/deny policy as the built-ins (see
  [Safety model](safety.md)).

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
