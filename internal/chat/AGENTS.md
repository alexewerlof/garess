# AGENTS.md — internal/chat

Google ADK `session.Service` backed by JSONL transcripts in
`.garess/sessions/`.

- `NewService(dir, historyLimit)` implements the 5-method ADK
  `session.Service` (Create/Get/List/Delete/AppendEvent). One `<id>.jsonl`
  file per session (a JSON-encoded `session.Event` per line) plus an
  `<id>.meta.json` sidecar (appName/userID/session state). The runner persists
  everything itself — the TUI never writes here.
- Conformance: the package runs ADK's
  `session/sessiontestsuite.RunServiceTests`. Requirements baked in: state
  scoping by `app:`/`user:`/session key prefixes, Create state survives Get,
  Get/List/Delete respect userID, Delete of non-existent/other-user sessions
  is a no-op, AppendEvent errors on missing sessions, **Partial events are
  NOT persisted**, and `temp:` state keys are stripped.
- `Get` caps events to `historyLimit` (the model context window; full
  transcript stays on disk). The runner never passes `NumRecentEvents`, so
  trimming is entirely this service's job.
- `NewSessionID()` / `NewEventID()` (crypto/rand) live in id.go.

## Context compaction (Phase 6)

- The model view can be compacted WITHOUT touching the transcript:
  `Service.Compact` (between turns) appends a summary event (author `user`,
  plain text) and records the latest `compaction{SummaryEventID,
CoveredThroughEventID, CoveredCount, Timestamp}` in the meta sidecar.
- `Get`/`List` then filter: model-visible = `[summary] + events after
CoveredThroughEventID` (the summary event itself is excluded from the
  tail), then `trim` keeps the summary sticky — the cap trims the tail, never
  the summary head. Each new compaction covers a prefix that includes the
  earlier summary, so only the latest marker is needed (older ones are
  subsumed). If the marker references missing events, Get fails open to raw
  history.
- The JSONL transcript is never rewritten or deleted by compaction — it
  stays the complete review record. `history_limit` still bounds event count
  on top of compaction.
