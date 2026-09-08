# AGENTS.md — internal/personas

Custom-agent definitions ("personas"): Markdown files with YAML frontmatter
describing specialized agents the main garess agent can run as sub-agents via
the `run_subagent` tool (see internal/harness/subagent.go). Mirrors the VS
Code custom-agent / Claude Code sub-agent format with garess-native roots.

- **Discovery** (`Discover(dir)`): user root `$XDG_CONFIG_HOME/garess/agents`
  first, then project roots `<dir>/.garess/agents` and `<dir>/agents`
  (capped at the launch dir, like AGENTS.md/skills). Every `*.md` file (hidden
  dirs skipped) is parsed; a file with no YAML `--- … ---` frontmatter or a
  malformed one is skipped with a warning, never fatal. Project personas
  override user personas of the same name (config merge-by-name rule).
- **Frontmatter** (`gopkg.in/yaml.v3`, unknown keys ignored): `name`
  (default = file stem), `description`, `tools` (YAML list OR comma string,
  e.g. `"read_file, grep"`), `model` (model-id override on the active
  provider), `user-invocable` (default true; parsed for future use — this
  milestone only runs personas as sub-agents), `disable-model-invocation`
  (default false; blocks sub-agent invocation), `agents` (sub-agent names this
  persona may delegate to: absent = any model-invocable, empty = none, list =
  those only — only consulted when `run_subagent` is in `tools`),
  `argument-hint`. The body is the persona's instruction text, rendered with
  `agents.RenderText` (@import/{{VAR}}).
- **Parse** (`Parse(path, scope) Persona`) validates the frontmatter and
  renders the body. `Validate(knownTools)` drops unknown tool names into
  `Warnings`. `ModelInvocable()` = !disable-model-invocation; `AllowSet()`
  converts the `agents` field to the allow set for run_subagent registration.
- Consumers: `cmd/garess` (discovery at startup → `harness.Options.Personas`,
  `garess doctor` `reportPersonas`) and `internal/harness` (run_subagent tool
  description + persona agent builds). Tests in `personas_test.go` cover
  frontmatter variants, body rendering, root precedence/override and
  malformed-file skipping.
