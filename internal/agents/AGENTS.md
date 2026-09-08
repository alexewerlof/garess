# AGENTS.md — internal/agents

Discovers and renders instruction files: AGENTS.md (agents.md convention)
plus a lightweight pi.dev-style `SYSTEM.md`. This is the engine that powers
the harness's own instruction-file support.

- **Discovery** (`Discover(dir)`): user files first —
  `$XDG_CONFIG_HOME/agents/AGENTS.md` then `~/.agents/AGENTS.md`, then the
  user `SYSTEM.md` at `$XDG_CONFIG_HOME/garess/SYSTEM.md` (pi.dev's
  `~/.pi/agent/SYSTEM.md` analog) — followed by ONLY `<dir>/AGENTS.md` and
  `<dir>/SYSTEM.md`. The project search is **capped at `dir`**: parent
  directories are never walked (deliberate — a stray root/parent file must not
  leak into model context). There is no `Depth` field anymore; keep it that way.
- **Rendering** (`Render(path)`): `@path/to/file` import lines must be the
  whole line (globs supported; cycles and missing files are errors; imports
  are relative to the file). Double-brace variables (e.g. a `VAR` placeholder)
  expand from the environment. `RenderText(text, baseDir)` runs the same
  expansion over inline text (skill and persona bodies use it; see
  `frontmatter.go` for the shared `SplitFrontmatter` delimiter helper).
- **Assembly** (`Build(dir)`): joins rendered sources with
  `## <BASENAME> (<scope> · <path>)` headers (AGENTS.md or SYSTEM.md).
- Consumers: `internal/tui` (`/agents`, `/agents reload`) and `cmd/garess`
  (`doctor`). If you change scope or ordering, update `agents_test.go` AND the
  injection/reload tests in `internal/tui/model_test.go`.
