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
- **Stream lifecycle.** `startStream(content)` starts a two-goroutine pump:
  a feeder drains the runner iterator (`runner.Run` +
  `runner.WithYieldUserMessage()`) into a raw channel, and a coalescer batches
  streaming (Partial) deltas and delivers them as ONE `adkEventMsg.batch` on a
  fixed `streamBatchInterval` (50ms) cadence, capped by `streamBatchCap` (64).
  Completed events and errors go through immediately. `handleADK` buffers a
  batch and `flushStream()`s right away — one frame per batch, not one per
  token (token-rate Views were the remaining RPi sluggishness). There is no
  render tick anymore. The FINAL non-partial event supersedes the partials
  (clears buffers, appends to `m.events` + `m.rendered`, `updateViewport`).
  `esc` cancels via context; the streamed partials stay on screen.
- **Rendering (incremental, cache-everything).** `m.events` (the display
  history) feeds `m.rendered` (cached ANSI, parallel) — completed events are
  rendered ONCE via `renderEvent` and appended; they are never re-rendered.
  The live assistant reply streams through `streamChunker`: each flush
  finalizes stable markdown chunks (split at blank lines outside code fences,
  capped at `assistantChunkCap` = 600 chars) and glamour-renders them once;
  only the small tail chunk is re-rendered per batch.
  **Do not call `renderAll()` on the streaming path** — it re-glamours the
  whole session and made the single-threaded Bubble Tea loop block for
  seconds as sessions grew (RPi 1: one token every few seconds, Esc dead).
  `renderAll()` is only for rare full refreshes (resize, `/new`, ctrl+t).
- **View layer (`convView`, NOT bubbles/viewport).** The conversation is
  rendered by `convView` (internal/tui/convview.go): a bottom-anchored,
  append-only line viewer. `updateViewport()` appends new stable parts
  (completed events, finalized chunks, ephemeral) ONCE — O(delta) — and
  replaces the bounded live tail (`streamTail`) in place — O(tail). Stable
  parts live in `conv.lines`, the live tail in `conv.live` (kept separate so
  appending stable content never freezes the old tail). `view()` is a plain
  join of the visible window (O(height)). Never reintroduce
  bubbles/viewport.SetContent: it re-splits and re-measures the WHOLE
  conversation per tick (O(session)) and its View() re-applies per-line
  lipgloss padding per frame — on a Pi 1 that alone was ~65–170ms per frame
  (GARESS_STATS `view` bucket) and 1s+ per SetContent. Scroll keys
  (up/down/pgup/pgdown/home/end) go through `Model.scroll` → convView.
  `updateViewport` does NOT force `gotoBottom()`: `setLive`/`clamp` keep the
  user's scroll position, and being at the bottom stays pinned automatically
  (yOff == 0). `startStream` force-scrolls to bottom so a new response is
  visible. Scrollback works both during and after streaming: page keys always
  scroll; arrows scroll when the conversation overflows the viewport, otherwise
  they edit the composer.
- **Per-frame lipgloss is the enemy (2026-09-02, Pi 1 measured).** Three
  separate O(frame) lipgloss costs were found and removed from the per-key
  path; do not reintroduce them:
  1. `lipgloss.JoinVertical` in `Model.View()` — re-measures the ANSI width of
     EVERY conversation line and re-pads the frame to a common width per call
     (~580µs of a 595µs View on a desktop; ~90ms/frame on a Pi 1). `View()` now
     does `strings.Join(header, conv, composer, status, "\n")` (left-aligned;
     an empty block still contributes one blank line, like JoinVertical).
  2. `lipgloss.NewStyle().Height(h).Render(...)` in `convView.view()` for the
     short-content case — a `Height` style makes lipgloss run its horizontal
     re-align pass (`alignTextHorizontal`), measuring/padding every line every
     frame. That fired whenever the conversation was shorter than the viewport
     (the normal chat case on tall terminals) and cost tens of ms/frame on the
     Pi. Short content is now padded with plain `strings.Repeat("\n", …)`.
  3. The composer (`textarea.View()` + `ui.composer.Render(...)`) is the last
     remaining per-key cost (~13–17ms on a Pi 1 at 118 cols; bubbles textarea
     re-renders through lipgloss + viewport with per-line width measurement
     every frame). Fixing it would require forking/replacing the bubbles
     textarea render (its position state is unexported); GARESS_STATS buckets
     (`view` + `tui-stats-view` hdr/conv/comp/status) are the measuring tools.
     Rule of thumb: anything that makes lipgloss measure or pad an already-built
     block on every frame is O(frame) and will hurt on the Pi.

- **HITL confirmation.** When a run yields an `adk_request_confirmation`
  FunctionCall, the run ends; `enterConfirmation` records the wrapper IDs and
  the TUI enters `m.confirming` (y/n prompt in the status line). `y`/`n`
  (`answerConfirmation`) resumes `startStream` with a FunctionResponse for
  every pending wrapper. Gotcha: `answerConfirmation` MUST return the model
  value produced by `startStream` (never a fresh copy).
- **Thinking blocks.** Model reasoning arrives as `genai.Part{Thought: true}`;
  hidden by default, `ctrl+t` toggles `m.showThinking` and re-renders.
  Streaming thinking is chunked too — `m.assistantThinking` is a
  `streamChunker` with a lipgloss renderer (`newStreamChunkerWith`), so only
  the thinking tail re-renders per batch. Never re-render the whole thinking
  per batch (that was `renderThinking(allThinking)` in the old `streamTail`)
  — with ctrl+t on it made updateViewport quadratic again while the model
  reasoned.
  **Thinking freezes when visible text starts** (`freezeThinking` in
  `flushStream`): the rendered thinking becomes `m.thinkingBlock`, a stable
  part placed BEFORE the response chunks in `stableParts`, and drops out of
  the live tail. Without this, finalized response paragraphs were appended
  ABOVE the tall live thinking block and got pushed out of view ("every new
  paragraph wipes out the previous text"). `toggleThinking` re-renders the
  frozen block when it was already frozen. `m.thinkingFrozen`/`m.thinkingBlock`
  are reset in startStream/completion/`/new`.
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

## Context usage + compression (Phase 6)

- See `context.go`: `Options` (passed into `New` — no opts = auto-compress on
  at 80%, fallback window 128k marked `≈`), `usage` estimate helpers, and the
  async compression flow (`startCompression` / `runCompression` /
  `handleCompressResult`, message `compressMsg`).
- Model fields: `ctxEst`/`ctxLastPrompt`/`ctxUsageThisRun` — exact
  `usage.prompt_tokens` is captured after every completed model event
  (`trackUsage`; the wire asks for it via `stream_options.include_usage`);
  when a run saw no usage, `finishStreaming` falls back to
  `estimatedPromptTokens()` (events + preamble, marked ≈). Also
  `ctxWindow`/`ctxApprox` (per-provider `windows` map),
  `compressing`/`compressIsAuto`, `pendingSend`/`pendingText` (pre-send
  auto-compress queue; restored to the textarea if cancelled),
  `summaryEventID`, `ctxNoAutoAfter` (anti-thrash watermark).
- The context readout is ALWAYS visible, right-aligned on the status line
  (`statusLine` → `contextReadout`: `ctx ≈12% · 30k/262k used · 232k free`;
  `≈` when the window is a fallback or the value is an unanchored estimate).
  It is NOT in the header. resize / /new / provider switch keep it consistent
  (`applyWindow`, resets in `newSession`).
- Checkpoints are TURN BOUNDARIES ONLY: `finishStreaming` (after each
  completed user↔assistant turn when the estimate is at/over the threshold)
  and a `send()` backstop when the context is ALREADY at/over
  `thresholdTokens()` (the new message itself is not compressible, so its
  size does not drive the trigger). NO mid-loop compression — the whole tool
  loop is one `runner.Run` and ADK v2.2.0 has no in-loop hook.
- `/compress [instructions]` + `/compact` (manual; runs regardless of usage);
  `/compress` requires ≥ 2 user messages (`compress.PickCutoff`) else
  "nothing to compress". Keys are gated while `compressing` (esc cancels,
  scroll/ctrl+t still work). While compressing, an in-conversation indicator
  (`compressionIndicator` — the conversation-area live block above the
  composer, the ONLY compression indicator; the status bar stays plain)
  shows activity with wording by `compressIsAuto` (manual = "compressing
  context", auto = "auto-compressing context"). The summary event renders as
  a distinct block via `renderSummary` (match `ev.ID == m.summaryEventID`),
  not a user bubble; the completion card reads `Context: X → Y tokens (freed
  Z) · usage a% → b%
of W` (`fmtCount` grouped numbers, `fmtPct` 1-decimal % under 10).
- Value-copy rules apply to compression too: the goroutine only reads
  snapshots captured at `startCompression` time (never the Model).
