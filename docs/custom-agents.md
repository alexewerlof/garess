# Custom agents & sub-agents

Custom agents let you define specialized personas — a planner, a security
reviewer, a code-quality agent — each with its own name, description, tool
allowlist, model and instructions. When you ask the main agent for something
complex, it can delegate a focused subtask to one of these personas by calling
the `run_subagent` tool, and the persona reports its final result back.

A sub-agent is an independent agent run: it gets its own context, tool set and
instructions, works autonomously through its own tool loop, and persists its
activity to its **own session transcript** under `.garess/sessions/` (so you
can audit exactly what it did).

## Agent files

Custom agents are Markdown files with a YAML frontmatter block. garess
discovers them from:

| Scope   | Location                                                       |
| ------- | -------------------------------------------------------------- |
| user    | `~/.config/garess/agents/*.md`                                 |
| project | `<launch dir>/.garess/agents/*.md`, `<launch dir>/agents/*.md` |

Project files override user files of the same name. Like `AGENTS.md`/skills,
the project search is capped at the launch directory. Run `garess doctor` to
list the personas currently in scope.

### Example

```markdown
---
name: researcher
description: Reads files and greps the codebase to answer focused questions.
tools: [read_file, grep, glob]
---

You answer questions about the codebase. Use read_file and grep to gather
evidence, then give a concise, well-grounded answer. Only report what the
code actually shows.
```

The body is the persona's instructions. It is injected (with the shared
AGENTS.md/SYSTEM.md/skills preamble) as the sub-agent's system instruction, and
supports `@import` lines and `{{VAR}}` environment substitution like other
instruction files.

### Frontmatter fields

| Field                      | Meaning                                                                                                                                                 |
| -------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `name`                     | The agent identifier (defaults to the file name). Case-sensitive — use it exactly in `run_subagent`.                                                    |
| `description`              | What the agent does. Shown to the main agent in the `run_subagent` tool description and in `doctor`.                                                    |
| `tools`                    | Tool allowlist. A YAML list or a comma string (`"read_file, grep"`). Empty = all built-in tools. Unknown names are ignored with a warning.              |
| `model`                    | Model-id override used with the active provider (same endpoint/API key). Empty = the main model.                                                        |
| `user-invocable`           | Default `true`. Reserved for future user-facing agent selection.                                                                                        |
| `disable-model-invocation` | Default `false`. Set `true` to prevent agents from invoking this persona via `run_subagent`.                                                            |
| `agents`                   | Sub-agent names this persona may itself delegate to (only consulted when `run_subagent` is in `tools`). Absent = any; `[]` = none; a list = only those. |
| `argument-hint`            | Optional hint for how to phrase the task when delegating to this agent.                                                                                 |

Only the fields above are read — any other YAML keys in the frontmatter are
ignored and never reach the model (skills get the same treatment).

### Tools

The `tools` list refers to the built-in tool names: `bash`, `read_file`,
`write_file`, `glob`, `grep`, `memory_read`, `memory_write`, `memory_list`,
`memory_delete`, and `run_subagent`. By default a sub-agent does **not** get
`run_subagent` (sub-agents cannot spawn sub-agents unless you explicitly list
it — the nesting depth is capped at 5). A persona with an empty `tools` list
gets all the built-ins.

## Using sub-agents

The main agent decides when delegation helps and calls `run_subagent` with the
persona name and a self-contained task. You don't invoke sub-agents directly —
hint at what you want, e.g.:

> Research how authentication is handled in this repo, then propose a plan.
> Use a sub-agent for the research so your main context stays focused.

Each `run_subagent` call is stateless: give the sub-agent all relevant context
and the exact expected output in the task.

### In the chat

While a sub-agent runs, the conversation shows a live row — collapsed by
default, with the persona name and its current tool:

```
▸ researcher · running grep …
```

When it finishes, a collapsible block appears between the ⚙ `run_subagent`
tool call and its ↳ result. It stays collapsed:

```
▸ researcher
```

Press `ctrl+o` to expand it (and collapse again) to see the task it was given,
every tool call and result it made, and its final result. (This mirrors the
`ctrl+t` thinking toggle.)

## Delegation & safety

- A persona only runs with the tools its `tools:` list allows, and its inner
  tool calls still pass through the same allow/ask/deny policy as the main
  agent. Because a sub-agent cannot answer a confirmation prompt, any inner
  call the policy marks **ask** fails closed (is denied) rather than pausing.
- `disable-model-invocation: true` keeps a persona out of the model's reach —
  it will not appear in the `run_subagent` tool description.
- Each run is recorded in its own session transcript, and the result text
  returned to the main agent is capped, so a chatty sub-agent cannot flood the
  conversation context.
