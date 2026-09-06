# AGENTS.md — cmd/garess

CLI entry point, flag parsing and subcommands.

- `main` → `run` parses flags then dispatches: TUI (no subcommand), `init`,
  `doctor`, `help`. `--provider`/`--model` override the config; `--config`
  overrides the search path. Undefined `-h`/`-help`/`--help` are reported by
  the flag package as `flag.ErrHelp` and print the matching usage (exit 0).
- Config is loaded via `loadConfig` (wraps `config.Load`); errors are converted
  to friendly, actionable messages by `friendlyConfigError`:
  - `*config.NotFoundError` → lists every path searched plus a create command.
  - `*config.InvalidConfigError` with `config.ErrNoProviders` → "config found
    but defines no LLM providers".
  - other invalid configs → "config <path> is invalid: <specific error>".
- `runTUI` builds one ADK harness provider per config provider
  (`harness.Build`), plus the memory store, the ADK session service
  (`chat.NewService`), a fresh session id, and a shared `harness.Preamble`,
  then passes everything to `tui.New`. `--model` mutates the current
  provider's config before building. `cfg.Hooks` and `cfg.MCPServers` flow
  through `harness.Options` (the latter → ADK toolsets, `internal/mcp`). The
  Phase 4 sandbox is applied last (`sandbox.Apply`, `GARESS_SANDBOX` env
  overrides the backend) right before `tui.New` — whole-process and
  irreversible, so after every directory exists; an explicit `landlock`
  backend that cannot apply aborts startup.
- `runDoctor` pings the default provider via `llm.Ping`, lists models via
  `llm.ListModels` (both hit `GET /v1/models`), and reports sandbox support
  (`sandbox.Available()` ABI probe), configured hooks (`reportHooks`),
  configured MCP servers with their tools (`reportMCP`, via
  `mcp.ListTools` — failures are printed, not fatal), and applicable
  AGENTS.md / SYSTEM.md files.
- `runInit` (`garess init`) writes the bundled `config.Example()` template to
  `./config.toml` (`-g` writes the global file at 0600, creating the dir; `-f`
  overwrites; an existing file is refused without `-f`).
- Structured logs go to a file (`~/.config/garess/garess.log`) via `slog` so
  the TUI owns stdout. Put new diagnostics in `doctor`, not on stdout.
- Context window resolution (Phase 6): `resolveContextWindows(cfg)` maps each
  provider to its window — configured `context_window` wins, else a bounded
  parallel `/v1/models` probe (`llm.LookupContextWindow`, 1.5s) — and feeds
  `tui.Options`. `runDoctor` calls `reportContext` to print auto-compress
  settings + each provider's resolved window/source.
