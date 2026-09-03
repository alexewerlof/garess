// Package harness wires a config provider into an ADK agent + runner: it
// builds the chat-completions model, the built-in functiontools, the system
// instruction (AGENTS.md/SYSTEM.md/skills preamble), the tool policy, and the
// tool-iteration cap.
package harness

import (
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"

	"garess/internal/config"
	"garess/internal/hooks"
	"garess/internal/llm"
	"garess/internal/memory"
	"garess/internal/tools"
)

// DefaultMaxToolIterations mirrors the pre-ADK harness cap.
const DefaultMaxToolIterations = 8

// AppName is the ADK app name garess sessions are stored under.
const AppName = "garess"

// Provider wraps one configured provider's ADK agent and runner.
type Provider struct {
	Name   string
	Model  string
	Agent  agent.Agent
	Runner *runner.Runner
}

// Options configures harness.Build.
type Options struct {
	// Memory is the note store backing the memory_* tools.
	Memory *memory.Store
	// WorkDir is the shell working directory for the bash tool.
	WorkDir string
	// SessionService is shared across providers (one per project).
	SessionService session.Service
	// Policy gates tool execution (deny/ask/allow).
	Policy tools.Policy
	// MaxToolIterations caps model calls per run (default
	// DefaultMaxToolIterations).
	MaxToolIterations int
	// Hooks are git-style shell hooks fired on agent/tool/model/session
	// events. Empty means no plugin is registered. See internal/hooks.
	Hooks []config.Hook
	// Preamble returns the current AGENTS.md/SYSTEM.md/skills instruction
	// text. It is re-evaluated on every run, so /agents reload just swaps the
	// backing data.
	Preamble func() (string, error)
	// Model, when set, overrides the model built from the provider config
	// (used by tests with a scripted model).
	Model model.LLM
}

// Build constructs the ADK agent and runner for a configured provider.
func Build(pc config.Provider, opts Options) (*Provider, error) {
	var m model.LLM
	if opts.Model != nil {
		m = opts.Model
	} else {
		built, err := llm.NewChatCompletionsModel(pc.Name, pc.Endpoint, config.ResolveAPIKey(&pc), pc.Model)
		if err != nil {
			return nil, err
		}
		m = built
	}
	builtinTools, err := tools.BuildTools(opts.Memory, opts.WorkDir, opts.Policy)
	if err != nil {
		return nil, fmt.Errorf("harness: build tools: %w", err)
	}

	maxIter := opts.MaxToolIterations
	if maxIter <= 0 {
		maxIter = DefaultMaxToolIterations
	}

	ag, err := llmagent.New(llmagent.Config{
		Name:        AppName,
		Description: "A local coding assistant that runs shell commands and inspects files.",
		Model:       m,
		Tools:       builtinTools,
		InstructionProvider: func(ctx agent.ReadonlyContext) (string, error) {
			if opts.Preamble == nil {
				return "", nil
			}
			return opts.Preamble()
		},
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{
			iterationCapCallback(maxIter + 1), // initial call + N tool iterations
		},
		BeforeToolCallbacks: []llmagent.BeforeToolCallback{
			tools.DenyCallback(opts.Policy),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("harness: build agent: %w", err)
	}

	// Optional git-style shell hooks, wired into the runner as a plugin.
	var pluginConfig runner.PluginConfig
	if len(opts.Hooks) > 0 {
		hs, err := hooks.Parse(opts.Hooks)
		if err != nil {
			return nil, fmt.Errorf("harness: hooks: %w", err)
		}
		hp, err := hooks.NewPlugin(hs)
		if err != nil {
			return nil, fmt.Errorf("harness: build hooks plugin: %w", err)
		}
		pluginConfig.Plugins = []*plugin.Plugin{hp}
	}

	r, err := runner.New(runner.Config{
		AppName:           AppName,
		Agent:             ag,
		SessionService:    opts.SessionService,
		AutoCreateSession: true,
		PluginConfig:      pluginConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("harness: build runner: %w", err)
	}
	return &Provider{Name: pc.Name, Model: pc.Model, Agent: ag, Runner: r}, nil
}

// iterationCapCallback caps the number of model calls per run (one invocation)
// so a misbehaving model cannot loop forever on tool calls or thinking. It
// counts via a temp: state key, which the session service strips on persist.
func iterationCapCallback(maxCalls int) llmagent.BeforeModelCallback {
	const key = "temp:garessModelCalls"
	return func(ctx agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
		if maxCalls <= 0 {
			return nil, nil
		}
		n := 0
		if v, err := ctx.State().Get(key); err == nil {
			if f, ok := v.(float64); ok {
				n = int(f)
			}
		}
		n++
		_ = ctx.State().Set(key, float64(n))
		if n > maxCalls {
			return nil, fmt.Errorf("agent stopped: reached the tool iteration limit (%d)", maxCalls-1)
		}
		return nil, nil
	}
}

// ToolNames returns the names of the built-in tools.
func ToolNames() []string { return tools.BuiltinNames() }
