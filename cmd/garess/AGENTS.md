# AGENTS.md — cmd/garess

CLI entry point, flag parsing and subcommands.

- `main` → `run` parses flags then dispatches: TUI (no subcommand), `doctor`,
  `help`. `--provider`/`--model` override the config; `--config` overrides the
  search path.
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
  provider's config before building.
- `runDoctor` pings the default provider via `llm.Ping`, lists models via
  `llm.ListModels` (both hit `GET /v1/models`), and reports sandbox support
  (`/sys/kernel/security/lsm`) and applicable AGENTS.md / SYSTEM.md files.
- Structured logs go to a file (`~/.config/garess/garess.log`) via `slog` so
  the TUI owns stdout. Put new diagnostics in `doctor`, not on stdout.
