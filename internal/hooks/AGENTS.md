# AGENTS.md — internal/hooks

Git-style shell hooks for garess agent events (Phase 3).

- **Config surface**: `[[hooks]]` entries in config.toml (`config.Hook`:
  `event`, `command`, `timeout`). Config is intentionally agnostic of event
  names — `Parse([]config.Hook)` validates them here. `internal/hooks` imports
  `internal/config` (never the reverse).
- **Invocation contract**: a hook runs as `sh -c <command> garess-hook <event>`
  so `$1` is the event name (also `GARESS_HOOK_EVENT`); the JSON `Payload`
  (see `payload.go`) is piped to stdin. stdout/stderr are captured — they
  never touch the TUI — and surface in logs and blocking errors. Per-hook
  timeout (default `DefaultTimeout` = 10s) bounds every run via
  `context.WithTimeout`; hook output is capped at `maxOutput` (64KB) by
  `capBuffer`. Timeouts kill the hook's **whole process group**: each hook
  runs with `SysProcAttr{Setpgid}` and `cmd.Cancel` sends SIGKILL to `-pid`
  (`run.go`), plus a 2s `WaitDelay` — otherwise only `sh` dies and an orphaned
  child (dash forks `sh -c "sleep 5"`) keeps the captured-output pipe open,
  blocking `cmd.Wait()` until it exits (found on the RPi: a 150ms timeout
  took 5s).
- **Blocking vs notification** (`blockingEvents`): non-zero exit **aborts** for
  `before_run`, `on_user_message`, `before_agent`, `before_model`,
  `before_tool` (returns an error; remaining hooks for that event are
  skipped). All others (`after_*`, `on_event`, `on_*_error`) are notifications:
  every hook runs and a failure is only `slog.Warn`ed. The plugin never
  mutates events/model payloads — hooks are observers.
- **Wiring**: `NewPlugin(*Set)` builds an ADK `plugin.Plugin` named
  `garess_hooks`, registering only the callbacks whose events have hooks
  (`plugin.go`). `harness.Build` adds it to `runner.Config.PluginConfig` when
  `Options.Hooks` is non-empty. `on_event` fires only on **non-partial**
  events — a per-delta hook would run once per streamed token.
- **Payload safety**: every string field and tool arg is rune-clipped
  (`clip`/`sanitizeArgs`) at the single choke point `finalSanitize` in
  `RunHooks`, so payloads stay a few KB even for `write_file` bodies or full
  replies. `fillContext` reads IDs from the ADK invocation context; the ADK
  invocation context is itself the `context.Context` hooks inherit (Esc during
  streaming cancels pending hooks).
- **Abort semantics through ADK**: `before_tool` failure becomes the tool's
  `{"error": ...}` result (like a policy deny) and the loop continues;
  `before_run` failure ends the run with an error; `before_model`/`before_agent`
  failures surface as model/agent errors. These are covered by the integration
  tests in `internal/harness/harness_test.go` (real `sh`, no network).
