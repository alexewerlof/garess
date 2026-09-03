# AGENTS.md — internal/skills

Skill discovery and rendering: optional per-topic instruction packs injected
into the model context alongside AGENTS.md/SYSTEM.md.

- **Discovery** (`Discover(dir)`): user roots first —
  `$XDG_CONFIG_HOME/garess/skills`, `$XDG_CONFIG_HOME/skills`,
  `~/.garess/skills`, `~/.skills` — then project roots `<dir>/.garess/skills`
  and `<dir>/skills`. Each root is walked recursively (`WalkDir`): any
  directory containing `SKILL.md` (or `README.md`) is a skill, named after
  its parent directory. Duplicate paths are dropped, and results are sorted
  by path.
- **Rendering** (`Render(path)`): delegates to `agents.Render`, so skills get
  the same `@import` expansion and `{{VAR}}` environment substitution as
  AGENTS.md. `Build(dir)` joins the discovered skills with
  `## Skill (<name> · <scope> · <path>)` headers.
- Consumers: `internal/tui` (`/skills`, `/skills reload`) and `cmd/garess`
  (`doctor`); the assembled text flows into the shared `harness.Preamble`
  like AGENTS.md/SYSTEM.md. Keep `skills_test.go` in sync when changing
  discovery or scope ordering.
