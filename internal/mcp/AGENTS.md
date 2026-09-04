# AGENTS.md — internal/mcp

MCP (Model Context Protocol) client support: turns `[[mcp_servers]]` config
entries into ADK toolsets so the agent can call external tools.

- `BuildToolsets(cfgs []config.MCPServer, policy tools.Policy)` builds one
  ADK `tool.Toolset` per configured server; empty input → empty output (the
  harness only calls it when `cfg.MCPServers` is non-empty, so there is zero
  overhead otherwise). `New(cfg, policy)` builds a single toolset.
- Transports (`config.MCPServer.Transport`): `stdio` (spawn
  `Command`/`Args`/`Env` via go-sdk `mcp.CommandTransport`), `sse`
  (`mcp.SSEClientTransport`), `http` (streamable HTTP
  `mcp.StreamableClientTransport`). Each toolset wraps an ADK
  `mcptoolset.New` with an explicit transport.
- **Auth:** every HTTP request carries `config.Headers`. The
  `GARESS_MCP_<NAME>_TOKEN` env var (NAME uppercased, non-alnum → `_`) sets
  `Authorization: Bearer <token>` and wins over an inline header (mirrors
  `GA_RESS_API_KEY`).
- **Degrade, don't fail:** ADK resolves toolset `Tools()` at the start of
  every run and an error there aborts the whole turn. `Server.Tools` catches
  discovery errors, `slog.Warn`s, and returns `(nil, nil)` — the run proceeds
  without that server's tools. `Server.Status()` exposes the last error for
  `garess doctor`. Startup never contacts servers (mcptoolset connects
  lazily).
- **Policy:** deny is enforced by the harness `tools.DenyCallback`
  (`BeforeToolCallback`, runs before every tool). `ask` is bound here as the
  mcptoolset `RequireConfirmationProvider` (`func(name, args) bool`) so MCP
  calls match the same `GARESS_TOOL_*` regexes as built-ins.
- `ListTools(ctx, cfg)` connects with a plain go-sdk client and returns the
  tool names — used by `garess doctor` (no ADK context needed there).
- Tests (`mcp_test.go`) spin a real SSE MCP server in-process with the go-sdk
  (`mcp.NewServer` + `mcp.AddTool` + `mcp.NewSSEHandler` over `httptest`) —
  never real networks.
