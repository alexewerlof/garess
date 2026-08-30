# AGENTS.md — internal/tui

Claude-Code-style terminal UI. Bubble Tea **v1** stack (bubbletea v1.3.10,
bubbles v1.0.0, lipgloss v1.1.0, glamour v1.0.0). Do not migrate to v2 —
`bubbles` only exists on the v1 API.

The agentic core is an ADK runner (`internal/harness`); the TUI pumps
`session.Event`s through a goroutine + channel and renders them. The runner
owns persistence.

Key rules (each one caused a real bug):

- **Mutable state + value copies.** Bubble Tea copies the Model on every
  Update. `strings.Builder` fields (`streamBuffer`, `thinkingBuffer`) MUST be
  `*strings.Builder` initialized in `New` — writing to a copied non-zero
  builder panics.
- **Focus at construction.** The composer `textarea` must be `.Focus()`ed in
  `New` (not `Init`, which runs on a value copy).
- **Stream lifecycle.** `startStream(content)` starts the pump goroutine
  (`runner.Run` + `runner.WithYieldUserMessage()`), returns `waitForADK` as
  the Cmd, and marks `m.streaming`. Partial events are buffered and flushed on
  a 50ms render tick; the FINAL non-partial event supersedes the partials
  (clears buffers, appends to `m.events`, `renderAll`). `esc` cancels via
  context.
- **Rendering.** `m.events` (the display history) feeds `m.rendered` (cached
  ANSI, parallel). `renderEvent` dispatches on content parts: user text,
  assistant text (+ hidden thinking via `renderThinking`), FunctionCall →
  tool block, FunctionResponse → result block, `adk_request_confirmation` →
  waiting marker. Markdown via glamour; ephemeral command output goes through
  `addInfo`.
- **HITL confirmation.** When a run yields an `adk_request_confirmation`
  FunctionCall, the run ends; `enterConfirmation` records the wrapper IDs and
  the TUI enters `m.confirming` (y/n prompt in the status line). `y`/`n`
  (`answerConfirmation`) resumes `startStream` with a FunctionResponse for
  every pending wrapper. Gotcha: `answerConfirmation` MUST return the model
  value produced by `startStream` (never a fresh copy).
- **Thinking blocks.** Model reasoning arrives as `genai.Part{Thought: true}`;
  hidden by default, `ctrl+t` toggles `m.showThinking` and re-renders.
- **Instructions.** AGENTS.md/SYSTEM.md + skills text is pushed into the
  shared `harness.Preamble` (read by the agent's InstructionProvider every
  run); `/agents reload` / `/skills reload` just update it — no rebuild.
- **Commands.** Slash commands live in `handleCommand`: `/help /new /quit
  /model /notes /agents /skills /tools`. Add new ones there and to `helpText`.

Testing: see `model_test.go`. Run programs with
`tea.NewProgram(m, tea.WithInput(blockingReader{}), tea.WithOutput(io.Discard))`
and inject keys via `prog.Send`; `prog.Run()` may return the final model as a
value OR a pointer (type-switch both). Persistence happens in the runner
before events reach the UI, so poll the session service then `drain()` before
sending `Quit`.
