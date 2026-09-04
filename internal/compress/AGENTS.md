# AGENTS.md — internal/compress

Context-compression engine: it turns an older slice of conversation events
into one dense summary so the agent keeps working inside its context window.
It only _prepares text and runs the sideband summarizer_ — persisting the
summary and a compaction marker is `chat.Service.Compact`'s job, and the raw
JSONL transcript is never modified here.

- `EstimateTokens(s)`: ≈1 token per 4 chars heuristic — there is no offline
  tokenizer in the pure-Go deps (and llama.cpp tokens are model-specific).
  The UI marks estimates and anchors them to the server's real
  `usage.prompt_tokens` on every model call; do NOT treat this as exact.
- `PickCutoff(events)`: the event index (inclusive) where summarization stops.
  The tail always starts at the LAST user-authored message, so the freshest
  exchange — and any tool call/response pairing inside it — survives
  verbatim. Returns false when there is nothing worth compressing (single
  exchange, or no user message).
- `Transcript(events, maxChars)`: flat role-prefixed transcript for the
  summarizer. Thinking (`Thought` parts) is omitted; FunctionCall /
  FunctionResponse parts render as `TOOL CALL:` / `TOOL RESULT:` lines with
  their JSON. `maxChars <= 0` disables the cap; past the cap the OLDEST
  content is dropped first (the marker explains).
- `SummaryPrompt(instruction, transcript)`: best-practice compression
  instructions — preserve goals/task, requirements+constraints, decisions and
  rationale, files/paths/commands touched and what changed, facts/answers
  from tool outputs, errors+fixes, open items; keep identifiers/numbers
  exact; fold in earlier summaries; output the digest only. Optional user
  instructions are appended verbatim.
- `Summarize(ctx, m model.LLM, system, user, maxOut)`: ONE non-streaming call
  (`stream=false`) with `Config.SystemInstruction` + `MaxOutputTokens`;
  returns the first completed response's text. Callers pass the provider's
  own `model.LLM` (`harness.Provider.LLMModel`), so tests inject the same
  scripted fake they use for the runner.
- Constants: `DefaultMaxTranscriptChars` (120k ≈ 30k tokens of history per
  summary call), `DefaultMaxSummaryTokens` (2000), `SummaryHeader` (prefixes
  the stored summary so the model can tell it from a live user message).

## Conventions

- Pure functions + one model call; no TUI, no session writes. Keep it free of
  bubbletea and chat imports so it stays unit-testable in isolation.
- Cutting between events must never split a user message from its tool chain
  — the `PickCutoff` "anchor on the last user message" rule is what
  guarantees it; don't switch to token-count cutoffs without re-checking
  that invariant.
- Tests use tiny fakes and a minimal `model.LLM` stub; no real network.
