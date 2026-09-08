package harness

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"garess/internal/chat"
	"garess/internal/config"
	"garess/internal/llm"
	"garess/internal/personas"
	"garess/internal/tools"
)

// RunSubagentTool is the function tool the main agent (and any persona that
// lists it) uses to delegate a focused subtask to a custom-agent persona.
const RunSubagentTool = "run_subagent"

// MaxSubAgentDepth caps how deep sub-agents may delegate to further
// sub-agents, preventing runaway recursion.
const MaxSubAgentDepth = 5

// localUserID is the single-user session scope (mirrors cmd/garess runTUI).
const localUserID = "local"

// subAgentResultChars bounds the result text a sub-agent run returns to the
// calling model, so a chatty sub-agent cannot blow the caller's context.
const subAgentResultChars = 32_000

// SubAgentSink receives live status updates from sub-agent runs so the TUI
// can render a running sub-agent (agent name, current tool) and collect its
// inner events for the expandable block. A nil sink means no observer.
type SubAgentSink func(SubAgentStatus)

// SubAgentPhase describes one live status update.
type SubAgentPhase int

const (
	// SubAgentStarted is emitted when a top-level sub-agent run begins.
	SubAgentStarted SubAgentPhase = iota
	// SubAgentTool reports an inner tool call about to execute.
	SubAgentTool
	// SubAgentEvent carries one completed inner event (for the expanded view).
	SubAgentEvent
	// SubAgentFinished carries the final result text.
	SubAgentFinished
	// SubAgentFailed carries the error text.
	SubAgentFailed
)

func (p SubAgentPhase) String() string {
	switch p {
	case SubAgentStarted:
		return "started"
	case SubAgentTool:
		return "tool"
	case SubAgentEvent:
		return "event"
	case SubAgentFinished:
		return "finished"
	case SubAgentFailed:
		return "failed"
	}
	return "unknown"
}

// SubAgentStatus is one live update emitted while a sub-agent runs.
type SubAgentStatus struct {
	// SessionID is the sub-agent's isolated session id and the TUI's record
	// key for the run.
	SessionID string
	// Agent is the persona name.
	Agent string
	// CallID is the run_subagent function-call id of the invoking tool call
	// (empty for non-top-level runs). The TUI anchors the block to that event.
	CallID string
	// Phase discriminates the payload fields below.
	Phase SubAgentPhase
	// Tool is the inner tool name (Phase SubAgentTool).
	Tool string
	// Event is a completed inner event (Phase SubAgentEvent / SubAgentTool).
	Event *session.Event
	// Result is the final result text (Phase SubAgentFinished).
	Result string
	// Err is the error text (Phase SubAgentFailed).
	Err string
}

// personaRuntime builds and caches persona agents and executes delegated
// sub-agent runs for one provider. All state is process-local and static for
// the lifetime of the process (personas are discovered at startup).
type personaRuntime struct {
	mu      sync.Mutex
	pc      config.Provider
	opts    Options
	main    model.LLM
	byName  map[string]personas.Persona
	ordered []personas.Persona // model-invocable personas, discovery order
	agents  map[string]agent.Agent
}

// newPersonaRuntime validates the given persona definitions, keeps only the
// model-invocable ones, and returns nil when none remain (no run_subagent
// tool should be registered).
func newPersonaRuntime(pc config.Provider, main model.LLM, opts Options, defs []personas.Persona) (*personaRuntime, error) {
	known := append(tools.BuiltinNames(), RunSubagentTool)
	rt := &personaRuntime{
		pc:     pc,
		opts:   opts,
		main:   main,
		byName: map[string]personas.Persona{},
		agents: map[string]agent.Agent{},
	}
	for _, p := range defs {
		if !p.ModelInvocable() {
			continue
		}
		p = p.Validate(known)
		for _, w := range p.Warnings {
			slog.Warn("harness: persona", "name", p.Name, "warning", w)
		}
		if _, dup := rt.byName[p.Name]; dup {
			slog.Warn("harness: duplicate persona", "name", p.Name)
			continue
		}
		rt.byName[p.Name] = p
		rt.ordered = append(rt.ordered, p)
	}
	if len(rt.ordered) == 0 {
		return nil, nil
	}
	return rt, nil
}

// buildRunSubagentTool builds a run_subagent function tool for an agent.
// allow filters which personas the caller may invoke (nil = all model-
// invocable personas); bindAsk wires the ask policy to HITL confirmation (the
// main agent) — nested agents cannot answer HITL and get bindAsk=false.
func (rt *personaRuntime) buildRunSubagentTool(allow map[string]bool, bindAsk bool) (tool.Tool, error) {
	cfg := functiontool.Config{
		Name:        RunSubagentTool,
		Description: rt.toolDescription(allow),
	}
	if bindAsk {
		cfg.RequireConfirmationProvider = tools.AskProvider[RunSubagentInput](rt.opts.Policy, RunSubagentTool)
	}
	return functiontool.New(cfg, func(ctx agent.Context, in RunSubagentInput) (map[string]any, error) {
		return rt.run(ctx, allow, in)
	})
}

// RunSubagentInput is the argument schema for the run_subagent tool.
type RunSubagentInput struct {
	Agent string `json:"agent" jsonschema:"Name of the sub-agent (custom agent persona) to delegate to"`
	Task  string `json:"task" jsonschema:"The full subtask for the sub-agent: include all relevant context and the exact expected output"`
}

// run executes one delegated sub-agent run and returns the tool result. It
// runs the persona as its own ADK runner against a fresh, isolated session,
// consuming the nested run's events synchronously and forwarding live status
// (for top-level runs only) to the sink.
func (rt *personaRuntime) run(ctx agent.Context, allow map[string]bool, in RunSubagentInput) (map[string]any, error) {
	persona, err := rt.lookup(allow, in.Agent)
	if err != nil {
		return nil, err
	}
	depth := subAgentDepth(ctx)
	if depth >= MaxSubAgentDepth {
		return nil, fmt.Errorf("sub-agent nesting depth limit (%d) reached", MaxSubAgentDepth)
	}
	// A top-level run is one invoked directly by the main agent; only those
	// create TUI records (they anchor to a run_subagent tool call in the main
	// conversation). Deeper runs surface inside their parent's expansion.
	topLevel := depth == 0
	sink := rt.opts.SubAgentSink
	if sink == nil {
		topLevel = false
	}
	callID := ""
	if topLevel {
		callID = ctx.FunctionCallID()
	}

	sessionID := chat.NewSessionID()
	if topLevel {
		sink(SubAgentStatus{SessionID: sessionID, Agent: persona.Name, CallID: callID, Phase: SubAgentStarted})
	}

	ag, err := rt.agentFor(persona)
	if err != nil {
		return nil, err
	}
	r, err := runner.New(runner.Config{
		AppName:           AppName,
		Agent:             ag,
		SessionService:    rt.opts.SessionService,
		AutoCreateSession: true,
	})
	if err != nil {
		return nil, fmt.Errorf("sub-agent %q runner: %w", persona.Name, err)
	}

	task := genai.NewContentFromText(in.Task, genai.RoleUser)
	nestedCtx := withSubAgentDepth(ctx, depth+1)

	var final strings.Builder
	var innerEvents int
	var runErr error
	for ev, eerr := range r.Run(nestedCtx, localUserID, sessionID, task, agent.RunConfig{StreamingMode: agent.StreamingModeSSE}) {
		if eerr != nil {
			runErr = eerr
			break
		}
		if ev == nil || ev.LLMResponse.Partial {
			continue // only completed events are persisted and shown
		}
		innerEvents++
		if topLevel {
			st := SubAgentStatus{SessionID: sessionID, Agent: persona.Name, CallID: callID, Event: ev}
			if name := functionCallName(ev); name != "" {
				st.Phase = SubAgentTool
				st.Tool = name
			} else {
				st.Phase = SubAgentEvent
			}
			sink(st)
		}
		if text := eventText(ev); text != "" {
			final.WriteString(text)
		}
	}
	if runErr != nil {
		if topLevel {
			sink(SubAgentStatus{SessionID: sessionID, Agent: persona.Name, CallID: callID, Phase: SubAgentFailed, Err: runErr.Error()})
		}
		return nil, fmt.Errorf("sub-agent %q failed: %w", persona.Name, runErr)
	}

	result := strings.TrimSpace(final.String())
	if result == "" {
		errText := fmt.Sprintf("sub-agent %q returned no final answer", persona.Name)
		if topLevel {
			sink(SubAgentStatus{SessionID: sessionID, Agent: persona.Name, CallID: callID, Phase: SubAgentFailed, Err: errText})
		}
		return nil, fmt.Errorf("%s", errText)
	}
	if topLevel {
		sink(SubAgentStatus{SessionID: sessionID, Agent: persona.Name, CallID: callID, Phase: SubAgentFinished, Result: result})
	}
	return map[string]any{
		"output":       capSubAgentResult(result),
		"session_id":   sessionID,
		"inner_events": innerEvents,
	}, nil
}

// lookup resolves a persona by name, checking model-invocability and the
// caller's allow set.
func (rt *personaRuntime) lookup(allow map[string]bool, name string) (personas.Persona, error) {
	p, ok := rt.byName[name]
	if !ok {
		var avail []string
		for _, q := range rt.ordered {
			if allow == nil || allow[q.Name] {
				avail = append(avail, q.Name)
			}
		}
		if len(avail) == 0 {
			return p, fmt.Errorf("unknown sub-agent %q (no sub-agents available)", name)
		}
		return p, fmt.Errorf("unknown sub-agent %q (available: %s)", name, strings.Join(avail, ", "))
	}
	if !p.ModelInvocable() {
		return p, fmt.Errorf("sub-agent %q cannot be invoked by agents", name)
	}
	if allow != nil && !allow[name] {
		return p, fmt.Errorf("sub-agent %q is not allowed for this agent", name)
	}
	return p, nil
}

// agentFor returns the cached persona agent for p, building it on first use.
func (rt *personaRuntime) agentFor(p personas.Persona) (agent.Agent, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if a, ok := rt.agents[p.Name]; ok {
		return a, nil
	}
	a, err := rt.buildAgent(p)
	if err != nil {
		return nil, err
	}
	rt.agents[p.Name] = a
	return a, nil
}

// buildAgent constructs the persona's llmagent: its own model (when the
// persona sets one), its tool allowlist (built-ins filtered to persona.Tools,
// plus run_subagent when listed), its instruction (base preamble + persona
// body), the shared deny/ask policy, and a per-run iteration cap.
func (rt *personaRuntime) buildAgent(p personas.Persona) (agent.Agent, error) {
	var m model.LLM = rt.main
	if p.Model != "" {
		built, err := llm.NewChatCompletionsModel(rt.pc.Name, rt.pc.Endpoint, config.ResolveAPIKey(&rt.pc), p.Model)
		if err != nil {
			return nil, fmt.Errorf("persona %q model: %w", p.Name, err)
		}
		m = built
	}

	personaTools, err := tools.BuildToolsFor(rt.opts.Memory, rt.opts.WorkDir, rt.opts.Policy, p.Tools)
	if err != nil {
		return nil, fmt.Errorf("persona %q tools: %w", p.Name, err)
	}

	// A persona can delegate further only when run_subagent is listed in its
	// tools; the allow set comes from its agents field (nil = any model-
	// invocable persona, empty list = none).
	if wantsTool(p.Tools, RunSubagentTool) {
		subTool, err := rt.buildRunSubagentTool(p.AllowSet(), false)
		if err != nil {
			return nil, fmt.Errorf("persona %q run_subagent tool: %w", p.Name, err)
		}
		personaTools = append(personaTools, subTool)
	}

	maxIter := rt.opts.MaxToolIterations
	if maxIter <= 0 {
		maxIter = DefaultMaxToolIterations
	}

	ag, err := llmagent.New(llmagent.Config{
		Name:        p.Name,
		Description: p.Description,
		Model:       m,
		Tools:       personaTools,
		InstructionProvider: func(ctx agent.ReadonlyContext) (string, error) {
			return rt.instructionFor(p)
		},
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{
			iterationCapCallback(maxIter + 1),
		},
		BeforeToolCallbacks: []llmagent.BeforeToolCallback{
			subagentGateCallback(rt.opts.Policy),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("persona %q agent: %w", p.Name, err)
	}
	return ag, nil
}

// toolDescription enumerates the sub-agents run_subagent can target for the
// caller, so the model picks the right persona.
func (rt *personaRuntime) toolDescription(allow map[string]bool) string {
	var names []string
	for _, p := range rt.ordered {
		if allow == nil || allow[p.Name] {
			names = append(names, p.Name)
		}
	}
	if len(names) == 0 {
		return "Delegate a focused subtask to a sub-agent. No sub-agents are available for this agent."
	}
	var sb strings.Builder
	sb.WriteString("Delegate a focused subtask to a sub-agent and return its final result. The sub-agent runs with its own context, tools and instructions; you receive only its final result. Include ALL relevant context and the exact expected output in the task. Available sub-agents:\n")
	for _, name := range names {
		p := rt.byName[name]
		desc := p.Description
		if desc == "" {
			desc = "(no description)"
		}
		sb.WriteString("- ")
		sb.WriteString(p.Name)
		if p.ArgumentHint != "" {
			sb.WriteString(" (task: ")
			sb.WriteString(p.ArgumentHint)
			sb.WriteString(")")
		}
		sb.WriteString(": ")
		sb.WriteString(desc)
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

// instructionFor builds the persona's system instruction: the shared base
// preamble (AGENTS.md/SYSTEM.md/skills) followed by the persona description,
// body and task rules.
func (rt *personaRuntime) instructionFor(p personas.Persona) (string, error) {
	var parts []string
	if rt.opts.Preamble != nil {
		base, err := rt.opts.Preamble()
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(base) != "" {
			parts = append(parts, strings.TrimSpace(base))
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "You are %q, a specialized sub-agent running a single delegated task.", p.Name)
	if p.Description != "" {
		sb.WriteString("\n\n")
		sb.WriteString(p.Description)
	}
	if p.Body != "" {
		sb.WriteString("\n\n")
		sb.WriteString(p.Body)
	}
	sb.WriteString("\n\nComplete the task you are given and return a concise final result. Work autonomously with your tools; do not ask the user questions.")
	parts = append(parts, sb.String())
	return strings.Join(parts, "\n\n---\n\n"), nil
}

// subagentGateCallback denies inner tool calls the policy marks Deny or Ask.
// Sub-agent runs cannot answer HITL confirmations, so an ask rule fails
// closed (denied) rather than pausing the run.
func subagentGateCallback(policy tools.Policy) llmagent.BeforeToolCallback {
	return func(ctx agent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
		switch policy.DecisionFor(t.Name(), args) {
		case tools.ApprovalDeny, tools.ApprovalAsk:
			return nil, fmt.Errorf("tool %q denied by policy", t.Name())
		}
		return nil, nil
	}
}

// ---- small helpers ---------------------------------------------------------

// subAgentDepthKey carries the sub-agent nesting depth through context.
type subAgentDepthKey struct{}

func withSubAgentDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, subAgentDepthKey{}, depth)
}

func subAgentDepth(ctx context.Context) int {
	if d, ok := ctx.Value(subAgentDepthKey{}).(int); ok {
		return d
	}
	return 0
}

func wantsTool(tools []string, name string) bool {
	for _, t := range tools {
		if t == name {
			return true
		}
	}
	return false
}

func functionCallName(ev *session.Event) string {
	if ev == nil || ev.Content == nil {
		return ""
	}
	for _, p := range ev.Content.Parts {
		if p.FunctionCall != nil {
			return p.FunctionCall.Name
		}
	}
	return ""
}

func eventText(ev *session.Event) string {
	if ev == nil || ev.Content == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range ev.Content.Parts {
		if p.Text != "" && !p.Thought {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func capSubAgentResult(s string) string {
	n := utf8.RuneCountInString(s)
	if n <= subAgentResultChars {
		return s
	}
	return string([]rune(s)[:subAgentResultChars]) +
		fmt.Sprintf("…[truncated: sub-agent result exceeds %d chars, %d omitted]", subAgentResultChars, n-subAgentResultChars)
}
