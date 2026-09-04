# AGENTS.md — internal/config

TOML configuration loading and validation.

- **Search order** (no explicit `--config`): `~/.config/garess/config.toml`
  (via `os.UserConfigDir()`) overlaid by the first existing of
  `.garess/config.toml` and `config.toml` (project root). Project wins;
  providers merge by name (`Merge`).
- **Defaults** are applied after merging: `tui.theme = "dark"`,
  `session.history_limit = 40`; `default_provider` falls back to the first
  provider.
- **Typed errors** — never return bare strings:
  - `*NotFoundError` (with `Searched` paths) when no config file exists.
  - `*InvalidConfigError` (`Path` + `Err`) for found-but-invalid files; the
    `ErrNoProviders` sentinel means the file defines no `[[providers]]`.
  - `cmd/garess` maps these to user-facing messages via `friendlyConfigError`.
- `ResolveAPIKey`: the `GA_RESS_API_KEY` env var wins over the `api_key`
  field.
- `[[hooks]]` entries (Phase 3) are stored as-is on `Config.Hooks`
  (`Hook{Event, Command, Timeout}`). Event names are hooks-package domain, so
  they are NOT validated here — `internal/hooks.Parse` does that when the
  runner is built. `Merge` applies project hooks per event (`mergeHooks`): a
  project hook list for an event replaces the global list for that event.
- `[sandbox]` (Phase 4): `Sandbox{Backend, WriteDirs}`; backend defaults to
  `none` (also set in `Default()`); `Validate` checks backend ∈
  none|auto|landlock and that `write_dirs` are absolute. Merge: project
  overrides backend and the whole write_dirs list when set.
- `[[mcp_servers]]`: `MCPServer{Name, Transport, URL, Command, Args, Env,
Headers}` stored as-is on `Config.MCPServers`. `Validate` checks unique
  names and transport-appropriate targets (`stdio` needs `command`;
  `sse`/`http` need a valid `url`); transport ∈ stdio|sse|http. Merge
  replaces a project/global server by name (like providers). Env/header
  semantics live in `internal/mcp`.
- When adding a field, keep `merge`, `applyDefaults`, `Validate`, and the
  tests in sync, and mirror the change in `example-config.toml` and
  `docs/configuration.md`.

## Context settings (Phase 6)

- `providers[].context_window` (int, tokens; 0 = unknown → probe `/v1/models`
  then `DefaultContextWindow` 128k). Provider merge replaces the whole
  provider block by name (as before).
- `session.auto_compress_threshold` is an **`*int`** so an explicit `0` can
  disable auto-compress while absence means the default (80). Merge copies
  the pointer when non-nil; `applyDefaults` materializes it when nil;
  `AutoCompressEnabled()` / `AutoCompressThresholdPct()` are the read
  helpers. `Validate` bounds it to 0..100 and rejects negative
  `context_window`.
