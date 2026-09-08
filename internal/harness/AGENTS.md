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
- **Sub-agent personas (`run_subagent`).** `opts.Personas
[]personas.Persona` (internal/personas, discovered by cmd/garess) enables
  delegation: when at least one model-invocable persona exists, `Build`
  appends a `run_subagent` function tool (name const `RunSubagentTool` in
  internal/harness/subagent.go) to the main agent. `subagent.go`'s
  `personaRuntime` builds/caches each persona as its own `llmagent` (model
  override `persona.Model` on the same provider, tool allowlist via
  `tools.BuildToolsFor`, instruction = base preamble + persona body, its own
  iteration cap, `subagentGateCallback` for deny/ask) and runs it as a NESTED
  ADK `runner.Run` against a fresh isolated session (`chat.NewSessionID()`),
  consuming its events synchronously — validated by
  `TestRunSubagentDelegatesAgenticLoop`. Sub-agents persist to their own JSONL
  session. Nesting: a persona only gets `run_subagent` if its `tools:` lists
  it (then gated by its `agents:` allowlist); depth is capped
  (`MaxSubAgentDepth`, context-carried). `opts.SubAgentSink` (a func) receives
  live `SubAgentStatus` updates (started/tool/event/finished/failed + inner
  events + the invoking `CallID`) for top-level runs only; the TUI renders
  them. Inner tools that policy marks ASK fail closed (denied) — nested runs
  cannot answer HITL.
- Tests (`harness_test.go`) drive the full loop against an `httptest` fake
  endpoint: text, tool call → execute → feed back, and the HITL confirmation
  two-`Run` round trip.
- `Provider` (Phase 6) also exposes `LLMModel model.LLM` (the built model,
  incl. the `opts.Model` test override — sideband calls like context
  compression reuse it) and `SessionService session.Service` (compression
  writes its summary + marker through it between turns). Both are set in
  `Build` from `opts`.
