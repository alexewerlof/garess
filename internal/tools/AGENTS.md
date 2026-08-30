# AGENTS.md — internal/tools

Built-in ADK functiontools and the allow/ask/deny approval gate.

- `BuildTools(mem, workDir, policy)` returns the functiontools: `bash`,
  `read_file`, `write_file`, `glob`, `grep`, `memory_read`, `memory_write`,
  `memory_list`, `memory_delete`. Handlers are `func(agent.Context, TArgs)
  (map[string]any, error)`; results carry an `output` (or `error`) key.
- Input structs need `json` tags AND a `jsonschema` tag whose value IS the
  description — **do not write `jsonschema:"description=..."`** (jsonschema-go
  v0.4.3 rejects the prefix). `functiontool.New` is generic; call it with
  concrete typed method values (never through `any`).
- Policy: `Policy{Allow, Ask, Deny []string}` of regexes, precedence
  `deny > ask > allow`. `DefaultPolicy()` reads `GARESS_TOOL_ALLOW` /
  `GARESS_TOOL_ASK` / `GARESS_TOOL_DENY` (`;`-separated). `DecisionFor(name,
  args)` builds the match string `name k1=v1 k2=v2` (sorted) so existing
  env regexes keep matching. **Non-empty policies default to deny.**
- Wiring: `ask` is enforced per-tool via `RequireConfirmationProvider` (ADK
  HITL); `deny` via `DenyCallback` (a `BeforeToolCallback`). Gotcha: a
  `BeforeToolCallback` returning a non-nil response short-circuits the tool
  and uses that response as the result — allowed calls MUST return
  `(nil, nil)`.
- No text wire protocol: tool calls use native OpenAI function calling.
