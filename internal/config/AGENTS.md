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
- When adding a field, keep `merge`, `applyDefaults`, `Validate`, and the
  tests in sync, and mirror the change in `example-config.toml` and the README.
