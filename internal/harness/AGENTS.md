# AGENTS.md — internal/harness

Wires a config provider into a Google ADK agent + runner.

- `Build(pc config.Provider, opts Options)` builds: the chat-completions
  `model.LLM` (or `opts.Model` in tests), the built-in functiontools
  (`tools.BuildTools`), an `llmagent` with `InstructionProvider` (reads the
  shared `Preamble`), `tools.DenyCallback` (deny policy), and an
  iteration-cap `BeforeModelCallback`. `opts.Preamble` is a `func() (string,
error)`; use `Preamble.Get` so `/agents reload` works without rebuilding.
- `opts.MCPServers` (`[]config.MCPServer`) are converted to ADK toolsets via
  `mcp.BuildToolsets` (each gated by the same ask policy) and registered as
  `llmagent.Config.Toolsets` — only when non-empty, so there is zero overhead
  with no `[[mcp_servers]]` configured. Deny is enforced by the shared
  `DenyCallback`. An unreachable server degrades to no tools for that turn
  (never fails the run).
- `Preamble` is a thread-safe holder for the system-instruction text
  (AGENTS.md/SYSTEM.md + skills).
- The iteration cap counts model calls per invocation via a `temp:garessModelCalls`
  state key (the session service strips `temp:` keys on persist). `Run` for an
  `llmagent` root goes through ADK's node runtime internally — that is the
  supported path.
- Session service, app name (`harness.AppName = "garess"`), and
  auto-create-session are configured here. The runner persists events; the
  TUI only renders them.
- `opts.Hooks` (`[]config.Hook`, Phase 3) are parsed by `internal/hooks.Parse`
  and registered on `runner.Config.PluginConfig` via `hooks.NewPlugin` when
  non-empty; otherwise no plugin is registered (zero per-event overhead).
  Hook abort semantics are exercised end-to-end in `harness_test.go`
  (`TestHookBeforeToolBlocksToolCall`, `TestHookBeforeRunAbortsRun`,
  `TestHookPassingStillRunsTool`) against the fake endpoint.
- Tests (`harness_test.go`) drive the full loop against an `httptest` fake
  endpoint: text, tool call → execute → feed back, and the HITL confirmation
  two-`Run` round trip.
