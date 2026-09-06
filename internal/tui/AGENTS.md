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
- **Thinking blocks (live, opencode/openrouter-style).** Model reasoning
  arrives as `genai.Part{Thought: true}`; the REASONING TEXT is hidden by
  default behind a live in-conversation `Thinking` header, and `ctrl+t`
  toggles `m.showThinking` to expand it (the thoughts stream live).
  `m.generationPending` (set in `startStream`, and after each tool-result
  event in `handleADK`; cleared when visible text starts, on a completed
  model/tool-call event, and in `finishStreaming`) drives the live slot:
  `updateViewport` shows `thinkingIndicator()` — an animated `Thinking …`
  placeholder before the first delta, then the real thinking block — until
  visible text arrives, so the indicator sits in the conversation right under
  the user's message instead of only in the status bar (status verb:
  `streamVerb()` thinking/responding/working). The placeholder animates via
  the spinner tick. Streaming thinking is chunked too — `m.assistantThinking`
  is a `streamChunker` with a lipgloss renderer (`newStreamChunkerWith`), so
  only the thinking tail re-renders per batch. Never re-render the whole
  thinking per batch (that was `renderThinking(allThinking)` in the old
  `streamTail`) — with ctrl+t on it made updateViewport quadratic again while
  the model reasoned.
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
- **Commands.** Slash commands live in the `commands.go` registry (single
  source of truth for `/help`, the slash palette and dispatch): `/help /new
/quit /model /notes /agents /skills /tools /sessions /compress`. Add new
  ones to the `commands` slice + attach `run` in `init()` (see the init-cycle
  gotcha below). `/sessions` opens the resume picker.

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

## Look & feel (2026-09-05) — hero, rails, palette, bottom bar

- **Theme-aware styles.** `styles.go` defines `darkColors`/`lightColors` +
  `buildStyles`; `resolveUI(theme)` (called from `New`) picks the active
  `ui` set (defaults to dark for tests). Warm-orange accent is the brand;
  user = blue rail, assistant = rose rail, internal blocks dim/plain.
- **Message rails = per-line zones.** `convView` keeps parallel
  `zones`/`liveZones` arrays; `appendStableZ`/`setLiveZ` tag content.
  `view()` prefixes railed lines with the precomputed ANSI rail
  (`railPrefix`) and SKIPS blank lines (message separators stay clean). No
  lipgloss/width math per line — keep it that way (Pi). Plain-zone
  `appendStable`/`setLive` remain for tests/benches. `stableParts()`
  returns `[]taggedPart`; `zoneForEvent` maps events (summary/tools → plain,
  user → user, assistant incl. thinking → assistant). `renderEvent` adds one
  trailing `\n` per completed event for spacing (see `renderEventInner`).
- **Hero empty state.** `hero()` is true when `len(m.rendered)==0` (and not
  busy). `View`/`frame` render `heroBlock()` + `statusLine`; heroBlock
  returns EXACTLY `height-1` rows (pixel logo via `logo.go`, tagline, editor
  panel via `editorBox`, hint), vertically centered. First message → normal
  layout.
- **Editor panel.** The prompt box wears the SAME left rail as user messages:
  `editorBox(w)` renders the panel at `w-ui.composerInner` and prefixes every
  row with `ui.composerRail` (the user rail painted over the panel
  background). The composer style has NO left padding (the rail owns those
  cells), so typed text stays at the conversation's column. Borderless: the
  panel is `Background(panelBG) + Padding(1,2,1,0)`, so the color difference
  is the boundary. `composerH` is 4 in `layout()` (bubbles textarea at height
  2 renders 2 rows + 2 padding rows). `ta.Prompt=""` removes the prompt
  glyph.
  `styleTextarea` (called in `New`) overrides the bubbles styles so the
  textarea blends into the panel. THREE things matter: (1) NO `CursorLine`
  block background (the textarea default paints an adaptive background
  behind the whole placeholder row — the old "darker highlight"); (2) the
  PANEL background is BAKED into every content style (`Text`, `CursorLine`,
  `Placeholder`, `EndOfBuffer`, `Prompt`) because the textarea emits style
  resets around its typed text — without the baked bg those rows fall back to
  the terminal's default (darker) background inside the lighter panel; and
  (3) the EMPTY composer never uses the textarea's own placeholder rendering
  (`composerBody()` draws the placeholder + blank row directly with
  `ui.placeholderBody` when `Value()==""`): bubbles `placeholderView` doesn't
  pad its short rows with a background, and the textarea's internal
  (unexported) viewport fills the rest of each row with PLAIN cells — a black
  band after the placeholder that the outer panel background cannot repaint.
- **Cursor hidden while the composer is empty.** bubbles renders its block
  cursor OVER the first placeholder character, and the reverse-video box
  reads as a black block next to the placeholder. `reconcileComposerCursor()`
  (called in `New`, after every textarea mutation in `handleKey`/`send`/
  `paletteComplete`, the compress-cancel restore and session resume) keeps
  the cursor in `cursor.CursorHide` mode while `Value()==""` and flips it to
  `cursor.CursorBlink` (returning the blink-start command) on the first typed
  character — so the placeholder renders clean and typing still gets the
  blinking block cursor. cursor.Model exposes `Mode()`; `SetMode(CursorBlink)`
  returns the cmd that (re)starts the blink cycle.
- **Slash palette.** Trigger: idle + single-line value starting with `/`.
  `paletteVisible()` hides when the typed word equals a full command (so
  `enter` still runs it — tests depend on this). Palette renders as the
  conversation live slot (`updateViewport`) or inside `heroBlock`;
  `handleKey` intercepts `↑↓/tab/enter/esc/ctrl+c` when visible; textarea
  edits re-arm (`paletteShow`) and refresh via `updateViewport`. Commands
  come from the `commands.go` registry (also drives `/help`); handlers are
  attached in `init()` because stored closures that call `helpText()` in the
  slice literal create a Go initialization cycle.
- **Type-ahead while busy (2026-09-06).** The composer stays editable while
  `m.streaming` (the harness busy state): `handleKey`'s streaming branch keeps
  `esc`/`ctrl+c` (cancel), page/scroll keys, `ctrl+t` and `ctrl+j` (newline),
  and routes `enter` to `queueWhileBusy()` — everything else goes through
  `editComposer(msg)` (the same helper the idle path uses; the palette
  re-arm is already idle-gated). `up`/`down` follow the idle overflow rule
  (scroll when `maxScroll()>0`, else edit the composer). `queueWhileBusy`
  trims the draft, clears the composer and stores `queuedContent`/`queuedText`
  (ONE slot: a second Enter while one is pending keeps the new draft in the
  composer rather than silently dropping the first; command lines are never
  queued). `finishStreaming` flushes the queue: on a clean completion the
  queued content is auto-sent via `startStream` (riding a post-turn
  auto-compress as `pendingSend` if one fires); on cancel/failure the text is
  restored to the composer instead of firing an unwanted run. The queue is NOT
  flushed while `confirming` — it waits for the confirmation-resumed run to
  finish. The status line appends `· next prompt queued` while one is pending.
- **Bottom bar.** `statusLine` = busy/error/`provider · model` left, `ctx`
  readout + `Options.Version` right. No top header anymore.
- **Type-ahead while busy (2026-09-06).** While `m.streaming` the composer
  stays editable — the user can draft the next prompt mid-turn. Control keys
  keep their streaming meaning (esc/ctrl+c cancel, ctrl+t thinking, scroll
  keys scroll; `up`/`down` scroll only when the conversation overflows, else
  they move the composer cursor, same rule as idle). `enter` calls
  `queueWhileBusy()`: it appends to `m.queued` (a FIFO `[]queuedPrompt`, cap
  `maxQueuedPrompts`; empty/command lines are never queued — command text
  stays in the composer) and clears the composer.
  `finishStreaming` pops the FRONT and auto-sends it via `startStream` on a
  clean finish (riding a post-turn auto-compress as `pending`), so queued
  prompts fire one per finished turn in submission order; remaining prompts
  stay queued. When the run was cancelled or failed, `restoreQueuedToComposer`
  returns the whole queue to the composer (newline-joined) instead of firing
  runs the user may not want — nothing is lost. The queue survives a
  confirmation round trip (finishStreaming's confirming early-return preserves
  it). The status bar shows `· N queued` while prompts are pending. Commands
  stay gated mid-run (the palette re-arm is guarded by `!m.streaming`).
- **Pending region (rendered 2026-09-06).** Queued prompts render as dim
  `Pending` blocks pinned between the conversation (whose live slot is the
  "current activity") and the composer: `frameParts()` stacks
  conv → pending → composer → status, and `joinFrame` takes a slice now.
  Rendering is `pendingRows(maxRows)` (maxRows = `pendingMaxRows()` =
  height − statusBarH − composerH − 1) — each prompt is a `Pending` label +
  word-wrapped body on the user rail (blue, it is the user's message), blank
  line between prompts, at most `pendingMaxShown` (3) expanded and the rest a
  `+N more` row; a single block taller than maxRows collapses to a count row
  so the region NEVER exceeds maxRows. `wrapPlain` does the wrapping (no
  glamour — cheap per-frame). Layout must agree with rendering: `layout()`
  computes `pendingH = len(pendingRows(pendingMaxRows()))` and shrinks
  `conv.height` by it, so queue mutations (`queueWhileBusy`, the
  `finishStreaming` flush/restore) call `layout()` + `updateViewport()`.
  `statusBarH`/`composerH` are package consts. Styles: `ui.pendingLabel`
  (italic dim) / `ui.pendingBody` (faint, PaddingLeft 2).

## Right session rail + resume (2026-09-05) — internal/tui/sessions.go

- The rail renders when `railActive()`: width >= `railMinWidth` (140) AND
  `sessionSvc != nil` (the concrete `*chat.Service` behind
  `Options.SessionService`). `contentWidth()` = width - `railWidth` (34) when
  active; the markdown renderer and all wrap widths use it, so content never
  runs under the rail. Per-frame width math (padding each frame line to the
  content width, `railFrame`) runs ONLY while the rail is active — desktop
  only, never at 118 cols on the Pi.
- Layout: `View`/`frame` compose the normal frame, then `wrapFrame` →
  `railFrame` lays it side by side with `railRows(height)` (full-height
  column: heading, current-session card, `past` list, bottom key hint). The
  hero empty state centers within the content column and lists past sessions
  the same way. Every rail row is `fit()` to `railWidth-2`; selected rows are
  painted with the accent background (`ui.railSel`).
- **List loading is async**: `loadSessions()` (tea Cmd) → `sessionsMsg` →
  `handleSessions` updates `m.sessions` (nil = not loaded yet → "loading…").
  Refreshed at Init, after each finished turn (`finishStreaming`), `/new`,
  and after a resume. `chat.Recent` (see internal/chat) backs it — newest
  first, preview = first user line, cap `railMaxSessions`.
- **Interaction.** `tab` (idle, rail active, entries exist) →
  `enterRailFocus`; while focused, `↑↓` move (`moveSessionsSel`), `enter`
  resumes (`resumeSelectedSession`), `esc`/`tab`/typing returns focus
  (`exitRailFocus` — clears the rail status hint). `/sessions` opens the same
  list as an in-conversation picker (`sessionsShow`, live slot in
  `updateViewport`, or inside `heroBlock`) that works at any width; typing or
  `esc` closes it.
- **Resume** (`resumeSession(id)`) loads `chat.Service.SessionView`
  (compaction-filtered + history-capped — the exact model view) into
  `m.events`/`m.rendered`, sets `m.sessionID` + `m.summaryEventID` and resets
  per-session state; the ADK runner `Get`s the existing session per Run (no
  rebuild). Display == what the model continues from. New turns append to the
  resumed session's JSONL.
- Value-copy rules apply: `sessions`/`railFocused` etc. are plain Model
  fields; async code only reads snapshots.
