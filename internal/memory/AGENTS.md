# AGENTS.md — internal/memory

Plain-markdown-file memory notes in two scopes: local (`.garess/memory/`) and
global (`~/.config/garess/memory/`).

- `SanitizeName` is the security boundary: names must match
  `[a-zA-Z0-9][a-zA-Z0-9._-]*`, contain no `/` or `\`, and not start with `.`;
  `.md` is appended when the name has no extension. Never bypass it (path
  traversal).
- `Read` checks local first (local shadows global) and returns a bool
  reporting the scope. `List` merges both scopes with local winning, sorted by
  name, and populates `Note.Path` (the TUI shows it in `/notes list`).
- Keep behavior covered by `store_test.go` (sanitize, shadowing, delete,
  traversal rejection).
