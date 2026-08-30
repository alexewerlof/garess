---
name: google-adk-go
description: 'Build AI agents in Go with Google Agent Development Kit (ADK) — google.golang.org/adk/v2. Use when implementing LLM-driven agents (llmagent), function tools (functiontool), workflow agents (sequentialagent/parallelagent/loopagent), Human-in-the-Loop (HITL) confirmation gates (toolconfirmation), custom model.LLM implementations, custom session.Service backends, runners, orchestrating sub-agents (transfer_to_agent), or integrating ADK into a Bubble Tea TUI.'
argument-hint: 'Describe the ADK Go agent, tool, or workflow to build'
---

# Google Agent Development Kit (ADK) for Go

Operational reference and implementation guide for AI coding agents orchestrating
and executing Go applications with Google's Agent Development Kit
(`google.golang.org/adk/v2`).

> Verified against adk-go v2.2.0 (2026-08-30). Some public APIs are marked
> EXPERIMENTAL; pin a version. Requires Go 1.26.5+.

## When to Use
- Implementing a new LLM-driven agent or multi-agent orchestration in Go
- Creating schema-validated function tools or HITL (Human-in-the-Loop) gated tools
- Composing deterministic workflows with sequential/parallel/loop agents
- Implementing a custom model.LLM (e.g. OpenAI-compatible / llama.cpp backends)
- Implementing a custom session.Service for file/JSONL/database persistence
- Wiring an ADK runner into a Bubble Tea TUI or async event loop
- Reviewing Go code for ADK API correctness, context handling, or state isolation

## 1. Package & Component Index
**Module Path:** `google.golang.org/adk/v2`

| Package | Key Types / Functions | Purpose |
| --- | --- | --- |
| `agent` | `agent.Agent` (interface; has an **unexported `internal()` method**, so build agents via `llmagent.New`, `agent.New`, or workflow constructors) | Core execution interface. |
| `agent/llmagent` | `llmagent.New(Config)` | LLM-driven agent: instruction, model, sub-agents, tools, callbacks. |
| `agent/workflowagents/sequentialagent` | `sequentialagent.New(Config)` | Deterministic pipeline running sub-agents sequentially. |
| `agent/workflowagents/parallelagent` | `parallelagent.New(Config)` | Concurrent agent execution using goroutines. |
| `agent/workflowagents/loopagent` | `loopagent.New(Config)` | Iterative loop agent (the only built-in place with a max-iteration concept). |
| `model` | `model.LLM` (interface), `model.Register`/`model.NewLLM` | Model abstraction; content is genai-typed end-to-end. |
| `model/gemini` | `gemini.NewModel(ctx, modelName, *genai.ClientConfig)` | Google Gemini / Vertex backend. |
| `model/openaimodel` | `openaimodel.NewModel(ctx, modelName, *ClientConfig{APIKey, BaseURL, HTTPClient, Options})` | OpenAI Responses API + OpenAI-compatible endpoints. **EXPERIMENTAL.** |
| `tool` / `tool/functiontool` | `tool.Tool` (interface), `functiontool.New(Config, Func[TArgs,TResults])` | Reflects Go structs to schema-validated function tools. |
| `tool/toolconfirmation` | `FunctionCallName`, `ToolConfirmation`, `OriginalCallFrom` | HITL confirmation (two-`Run` round trip). |
| `tool` | `tool.WithConfirmation(Toolset, ...)` | Toolset-level confirmation gate. **EXPERIMENTAL.** |
| `session` | `session.Service` (interface), `session.InMemoryService()`, `session/sessiontestsuite.RunServiceTests` | Conversation state, history, persistent variables. |
| `session/database` | `database.NewSessionService(gorm.Dialector, ...)` | GORM-backed sessions (Postgres/MySQL/SQLite). |
| `runner` | `runner.New(Config)`, `r.Run(ctx, userID, sessionID, msg, cfg, opts...) iter.Seq2[*session.Event, error]` | Orchestrates turns, the tool-call loop, state persistence, event streaming. |
| `plugin` | `plugin.New(Config)` | Cross-cutting callbacks (model/tool/session events) wired into the runner. |
| `memory` | `memory.Service` (interface), `memory.InMemoryService()` | Long-term cross-session memory search. |

## 2. Core Execution Topology & Flow

### 2.1 Runner Turn Loop

```
[User Content] -> runner.Run(ctx, userID, sessionID, msg *genai.Content, cfg)  (iter.Seq2)
                        |  loads session history
                        v
                    llmagent --> model.LLM.GenerateContent (genai content)
                        |   ^
                        |   +-- tool result (FunctionResponse) fed back
                        v
               automatic tool-call loop: execute FunctionCalls (parallel via
               goroutines) -> feed results back -> call the model again
                        v
               yield session.Events -> persist non-partial events -> stop on
               IsFinalResponse()
```

- **Message input is `*genai.Content`**, not a string: build with
  `genai.NewContentFromText(text, genai.RoleUser)`.
- **`runner.Run` signature** (v2.2.0):
  `Run(ctx context.Context, userID, sessionID string, msg *genai.Content, cfg agent.RunConfig, opts ...RunOption) iter.Seq2[*session.Event, error]`.
  `agent.RunConfig{StreamingMode: agent.StreamingModeSSE}` yields partial deltas;
  `RunOption.WithStateDelta(...)`, `WithYieldUserMessage()`.
- **The tool loop is automatic and has NO max-iteration cap** — the internal
  `Flow.Run` is a bare `for {}` until `IsFinalResponse()`. Impose your own cap
  (e.g. a `BeforeModelCallback` counting calls) if you need one.
- **Events:** `session.Event` embeds `model.LLMResponse` (so `Content`,
  `Partial`, `UsageMetadata` come free) plus `ID`, `Timestamp`, `InvocationID`,
  `Branch`, `Author` ("user" or agent name), `Actions` (StateDelta,
  ArtifactDelta, RequestedToolConfirmations, SkipSummarization,
  TransferToAgent, Escalate), `LongRunningToolIDs`. There is **no `Role`
  field** — the role lives in `Event.Content.Role`. `IsFinalResponse()` tells
  you when the loop stops.
- **Reasoning/thinking** rides on `genai.Part{Thought: true}`; ADK does not
  hide it — hiding is the UI's job.

### 2.2 Orchestration Patterns

- **Dynamic Delegation (Coordinator):** Root `LLMAgent` holds sub-agents in
  `SubAgents: []agent.Agent`. The LLM dynamically delegates using the implicit
  `transfer_to_agent` system tool.
- **Deterministic Sequence:** `sequentialagent` passes the output of `Step[N]`
  into the input context for `Step[N+1]`.
- **Parallel Fan-Out / Aggregation:** `parallelagent` executes all `SubAgents`
  concurrently, returning consolidated event streams.

## 3. Implementation Patterns

### 3.1 Typed Tool & Single Agent Setup

```go
package main

import (
    "context"

    "google.golang.org/adk/v2/agent"
    "google.golang.org/adk/v2/agent/llmagent"
    "google.golang.org/adk/v2/model/gemini"
    "google.golang.org/adk/v2/runner"
    "google.golang.org/adk/v2/session"
    "google.golang.org/adk/v2/tool"
    "google.golang.org/adk/v2/tool/functiontool"
    "google.golang.org/genai"
)

type MetricsInput struct {
    Service string `json:"service" jsonschema:"Service name to query"`
}

type MetricsOutput struct {
    LatencyMS float64 `json:"latency_ms"`
    ErrorRate float64 `json:"error_rate"`
}

func getMetrics(ctx agent.Context, in MetricsInput) (MetricsOutput, error) {
    return MetricsOutput{LatencyMS: 42.1, ErrorRate: 0.001}, nil
}

func NewOpsAgent(ctx context.Context) (*runner.Runner, session.Service, error) {
    t, err := functiontool.New(functiontool.Config{
        Name:        "get_metrics",
        Description: "Fetches latency and error rate metrics for a service.",
    }, getMetrics) // generic handler: func(agent.Context, TArgs) (TResults, error)
    if err != nil {
        return nil, nil, err
    }

    m, err := gemini.NewModel(ctx, "gemini-2.0-flash", &genai.ClientConfig{
        APIKey: os.Getenv("GOOGLE_API_KEY"),
    })
    if err != nil {
        return nil, nil, err
    }

    ag, err := llmagent.New(llmagent.Config{
        Name:        "ops_agent",
        Instruction: "Monitor system health using get_metrics.",
        Model:       m,
        Tools:       []tool.Tool{t},
    })
    if err != nil {
        return nil, nil, err
    }

    sSvc := session.InMemoryService() // NOTE: not NewInMemoryService()
    r := runner.New(runner.Config{Agent: ag, SessionService: sSvc})
    return r, sSvc, nil
}
```

### 3.2 Dynamic Delegation (Supervisor + Specialists)

```go
package main

import (
    "context"

    "google.golang.org/adk/v2/agent"
    "google.golang.org/adk/v2/agent/llmagent"
    "google.golang.org/adk/v2/model"
    "google.golang.org/adk/v2/tool"
)

func NewCoordinator(ctx context.Context, m model.LLM, tools ...tool.Tool) (agent.Agent, error) {
    secAgent, err := llmagent.New(llmagent.Config{
        Name:        "security_specialist",
        Instruction: "Audit configurations for security risks and open ports.",
        Model:       m,
        Tools:       tools,
    })
    if err != nil {
        return nil, err
    }

    return llmagent.New(llmagent.Config{
        Name:        "triage_root",
        Instruction: "Route security inquiries to security_specialist.",
        Model:       m,
        SubAgents:   []agent.Agent{secAgent}, // Enables transfer_to_agent auto-routing
    })
}
```

### 3.3 Deterministic Workflow Composition (Sequential & Parallel)

```go
package main

import (
    "google.golang.org/adk/v2/agent"
    "google.golang.org/adk/v2/agent/workflowagents/parallelagent"
    "google.golang.org/adk/v2/agent/workflowagents/sequentialagent"
)

func NewAuditPipeline(ingest, validate, analyzeA, analyzeB agent.Agent) (agent.Agent, error) {
    // Concurrent analysis branch
    parallelAnalysis, err := parallelagent.New(parallelagent.Config{
        Name:      "concurrent_analysis",
        SubAgents: []agent.Agent{analyzeA, analyzeB},
    })
    if err != nil {
        return nil, err
    }

    // Rigid sequence: Ingest -> Validate -> Parallel Analysis
    return sequentialagent.New(sequentialagent.Config{
        Name:      "audit_pipeline",
        SubAgents: []agent.Agent{ingest, validate, parallelAnalysis},
    })
}
```

### 3.4 Human-In-The-Loop (HITL) Tool Gating — TWO-RUN ROUND TRIP

Configure per-tool confirmation on `functiontool.Config`:
- `RequireConfirmation bool` — always ask
- `RequireConfirmationProvider any` — must be `func(TArgs) bool`; return true to ask for this call

When a tool requires confirmation it returns `tool.ErrConfirmationRequired`
(NOT executed). The current `runner.Run` then **ends**, after yielding 3 events:
1. the model's FunctionCall event,
2. a placeholder FunctionResponse event `{"error": "...requires confirmation..."}`,
3. a `toolconfirmation.FunctionCallName` ("adk_request_confirmation")
   FunctionCall event — wrapper ID, original call via
   `toolconfirmation.OriginalCallFrom(fc)`, prompt hint in
   `fc.Args["toolConfirmation"].(toolconfirmation.ToolConfirmation).Hint`.

The app replies by calling `runner.Run` **again** on the same
userID/sessionID with a user content carrying the FunctionResponse:

```go
funcResponse := &genai.FunctionResponse{
    Name:     toolconfirmation.FunctionCallName,
    ID:       wrapperCallID,          // the wrapper FC ID from the event
    Response: map[string]any{"confirmed": approved},
}
appResponse := &genai.Content{
    Role:  string(genai.RoleUser),
    Parts: []*genai.Part{{FunctionResponse: funcResponse}},
}
// for ev := range r.Run(ctx, userID, sessionID, appResponse, cfg) { ... }
```

The internal `RequestConfirmationRequestProcessor` (preprocess of the resumed
run) matches the wrapper ID back to the original call and re-dispatches the
tool with `ctx.ToolConfirmation()` set — `Confirmed=true` runs it,
`Confirmed=false` yields `tool.ErrConfirmationRejected`. Multiple pending
wrapper calls (parallel) can be answered in one resume message.

Canonical patterns: `examples/toolconfirmation/main.go` and
`internal/llminternal/parallel_function_call_hitl_test.go`.

**Note:** `tool.WithConfirmation(ts Toolset, requireConfirmation bool, provider ConfirmationProvider) Toolset`
exists but is **EXPERIMENTAL** and takes a Toolset, not a single tool.

### 3.5 Asynchronous TUI Integration (Bubble Tea) — EVENT PUMP

`runner.Run` returns a **pull-based `iter.Seq2`** — you cannot pause it from a
Bubble Tea `Update`. Pump it through a goroutine into a channel and read one
event per tea.Msg:

```go
type adkEventMsg struct {
    ev  *session.Event
    err error
}

func PumpADK(r *runner.Runner, userID, sessionID string, content *genai.Content, cfg agent.RunConfig) (tea.Cmd, context.CancelFunc) {
    ctx, cancel := context.WithCancel(context.Background())
    ch := make(chan adkEventMsg, 1)
    go func() {
        defer close(ch)
        for ev, err := range r.Run(ctx, userID, sessionID, content, cfg) {
            ch <- adkEventMsg{ev, err}
            if err != nil {
                return
            }
        }
    }()
    return func() tea.Msg { return <-ch }, cancel
}
```

Handle each event:
- `ev.LLMResponse.Partial` with text parts -> stream buffer (render live)
- parts with `Thought: true` -> separate hidden "thinking" buffer (UI choice)
- parts with `FunctionCall` (name != `adk_request_confirmation`) -> render tool request
- parts with `FunctionResponse` -> render tool result
- `adk_request_confirmation` FunctionCall -> prompt user, then start a new
  `Run` with the confirmation FunctionResponse (see 3.4)
- `ev.IsFinalResponse()` / iterator end -> turn complete

**Gotcha:** return the `(tea.Model, tea.Cmd)` produced by the new `Run` from
your Update handler — never start a stream on a separate model copy.

## 4. Custom Model Implementation (`model.LLM`)

The whole `model.LLM` interface is two methods:

```go
type LLM interface {
    Name() string
    GenerateContent(ctx context.Context, req *LLMRequest, stream bool) iter.Seq2[*LLMResponse, error]
}
```

- Request/response are **genai-typed**: `LLMRequest{Model string, Contents []*genai.Content, Config *genai.GenerateContentConfig, Tools map[string]any}`; `LLMResponse{Content *genai.Content, UsageMetadata, FinishReason, Partial bool, TurnComplete bool, ...}`.
- Function calls arrive in `LLMResponse.Content.Parts[i].FunctionCall` (`*genai.FunctionCall{ID, Name string, Args map[string]any}`). Tool results are fed back as user-role content with `Part.FunctionResponse` (carries `ID`).
- Streaming contract: with `stream=true`, yield partial `LLMResponse{Partial: true}` deltas, then one final `LLMResponse{Partial: false, TurnComplete: true}`.
- No `model.BaseModel` helper exists; reference templates are `model/gemini/gemini.go` and `model/openaimodel/openai.go`.
- Optional structural interfaces (implement to opt in): `GetGoogleLLMVariant() genai.Backend`, `Client() *genai.Client` (Live only).
- For an OpenAI-compatible / llama.cpp backend: map genai content -> chat messages (assistant `tool_calls[]` must carry the FULL arguments JSON; FunctionResponse -> `tool` role with `tool_call_id = fr.ID`), and parse reasoning (`reasoning_content`) into `genai.Part{Thought: true}`.

## 5. Custom Session Persistence (`session.Service`)

```go
type Service interface {
    Create(context.Context, *CreateRequest) (*CreateResponse, error)
    Get(context.Context, *GetRequest) (*GetResponse, error)
    List(context.Context, *ListRequest) (*ListResponse, error)
    Delete(context.Context, *DeleteRequest) error
    AppendEvent(context.Context, Session, *Event) error
}
```

- `session.Session` is an **interface**: `ID()`, `AppName()`, `UserID()`, `State() State`, `Events() Events`, `LastUpdateTime() time.Time`.
- Shipped backends: `session.InMemoryService()` and `session/database` (GORM). To persist to files/JSONL, implement `Service` yourself and validate it with the conformance suite: `session/sessiontestsuite.RunServiceTests(t, opts, setup)`.
- State scopes: `app:` (cross-session), `user:` (per-user), `temp:` (per-invocation, discarded). Tools mutate via `ctx.Actions().StateDelta` or `ctx.State().Set`.

## 6. Callbacks & Plugins

Per-agent callbacks (fields of `llmagent.Config`):

```go
type BeforeModelCallback  func(ctx agent.Context, req *model.LLMRequest) (*model.LLMResponse, error)
type AfterModelCallback   func(ctx agent.Context, resp *model.LLMResponse, err error) (*model.LLMResponse, error)
type OnModelErrorCallback func(ctx agent.Context, req *model.LLMRequest, err error) (*model.LLMResponse, error)
type BeforeToolCallback   func(ctx agent.Context, t tool.Tool, args map[string]any) (map[string]any, error)
type AfterToolCallback    func(ctx agent.Context, t tool.Tool, args, result map[string]any, err error) (map[string]any, error)
type OnToolErrorCallback  func(ctx agent.Context, t tool.Tool, args map[string]any, err error) (map[string]any, error)
```

`Before*` callbacks short-circuit: a non-nil return skips the real call. Agent-level hooks: `agent.BeforeAgentCallback` / `agent.AfterAgentCallback`.

Cross-cutting hooks: `plugin.New(plugin.Config{...})` (same callback families + `OnUserMessageCallback`, `OnEventCallback`, `BeforeRunCallback`, `AfterRunCallback`, `CloseFunc`) wired via `runner.Config.PluginConfig`.

## 7. Long-Term Memory (`memory.Service`)

```go
type Service interface {
    AddSessionToMemory(ctx context.Context, s session.Session) error
    SearchMemory(ctx context.Context, req *SearchRequest) (*SearchResponse, error)
}
```

Shipped: `memory.InMemoryService()` (keyword search) and `memory/vertexai`. Wired via `runner.Config.MemoryService`; exposed to the model via the built-in `load_memory`/`preload_memory` tools and to tools via `ctx.SearchMemory(query)`. No file backend is shipped — implement it, or keep your own memory tools as plain functiontools.

## 8. Strict Engineering Rules for AI Generators

- **Type Definition:** Tool struct fields MUST include `json` and
  `jsonschema:"..."` tags for clean LLM tool schema generation. The jsonschema
  tag IS the description — do NOT prefix it with `description=` (jsonschema-go
  v0.4.3 rejects tags matching `^[^ \t\n]*=`).
- **Tool signature:** functiontool handlers are `func(agent.Context, TArgs) (TResults, error)` — the first parameter is `agent.Context` (which embeds `context.Context`), NOT a bare `context.Context`. Never `panic()`; return clean errors.
- **BeforeToolCallback:** a non-nil `(response, nil)` return short-circuits the tool and becomes its result — allowed calls must return `(nil, nil)`.
- **Context Cancellation:** enforce deadlines/cancellations on network calls in tools and models.
- **State Isolation Tiers:** `InvocationState` (temp:), `SessionState` (user:), `AppState` (app:).
- **No Direct Prompt Control for Workflows:** Use `sequentialagent`/`parallelagent` for exact order; do not enforce sequence via prompts.
- **Max iterations:** the LLM-agent loop has NO built-in cap — add one via `BeforeModelCallback` if needed.

## 9. Versioning & Gotchas

- Pin `google.golang.org/adk/v2` (v2.2.0 verified 2026-08-30); requires Go 1.26.5+.
- **EXPERIMENTAL:** `model/openaimodel` (OpenAI Responses API), `tool.WithConfirmation`.
- Stale APIs to avoid: `session.NewInMemoryService()` -> `session.InMemoryService()`; `model.NewGoogleModel` -> `gemini.NewModel`; `functiontool.Config.Fn` -> handler argument; `runner.Run(ctx, sessionID, userID, prompt)` -> `Run(ctx, userID, sessionID, msg, cfg)`; `toolconfirmation.WithConfirmation(tool.Tool, ...)` -> `tool.WithConfirmation(Toolset, ...)` or per-tool `RequireConfirmation`.
- Dependencies are pure-Go (genai, jsonschema-go; openai-go only via openaimodel; gorm/glebarez-sqlite only via session/database) — cross-compiles to linux/arm GOARM=6, but binaries are large (tens of MB).
- ADK 2.0 also ships a newer "node" runtime (`runner/run_node.go`, `workflow/`); the documented `llmagent` + `runner.Run` path is the stable one.

## 10. References

- https://adk.dev/get-started/go/
- https://adk.dev/get-started/about/
- https://adk.dev/tools-custom/function-tools/
- https://adk.dev/tools-custom/mcp-tools/
- https://adk.dev/callbacks/
- https://adk.dev/plugins/
- https://adk.dev/agents/models/openai/ (openaimodel, OpenAI-compatible endpoints)
- https://pkg.go.dev/google.golang.org/adk/v2
- Source: https://github.com/google/adk-go (examples/, internal/llminternal, session/sessiontestsuite)
