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
- **Frontmatter (parsed, filtered, stripped).** A skill file MAY start with a
  YAML `--- … ---` block (shared `agents.SplitFrontmatter`). It is parsed
  with `gopkg.in/yaml.v3` for `name` and `description` only; UNKNOWN KEYS ARE
  IGNORED and the block is stripped, so arbitrary YAML in a skill never
  reaches the model. `Parse(path, scope)` returns a `Skill{Name, Description,
Path, Scope, Body}` (Body = frontmatter-stripped, `@import`/`{{VAR}}`
  expanded). A malformed frontmatter block skips that file with a warning
  (one broken skill never hides the rest); files without frontmatter keep
  their full body. `Discover` returns `[]Skill` (already parsed); `Build`
  renders each as `## Skill (<name> · <scope> · <path>)` + optional
  description + body; `Render(path)` returns just the body.
- Consumers: `internal/tui` (`/skills`, `/skills reload`) and `cmd/garess`
  (`doctor`); the assembled text flows into the shared `harness.Preamble`
  like AGENTS.md/SYSTEM.md. Keep `skills_test.go` in sync when changing
  discovery or scope ordering.
