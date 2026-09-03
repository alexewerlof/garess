package hooks

import (
	"strings"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
)

// NewPlugin builds an ADK plugin that dispatches runner events to the
// configured shell hooks. Only the callbacks for events that have at least one
// configured hook are registered, so an empty Set adds no per-event overhead.
//
// The plugin never modifies events or model/tool payloads — hooks are
// observers. Blocking events abort by returning an error from the callback
// (which the ADK runner surfaces as the run/tool/model error); notification
// events log failures internally and always pass through.
func NewPlugin(s *Set) (*plugin.Plugin, error) {
	cfg := plugin.Config{Name: "garess_hooks"}

	if s.Has(EventBeforeRun) {
		cfg.BeforeRunCallback = func(ictx agent.InvocationContext) (*genai.Content, error) {
			return nil, s.fire(ictx, EventBeforeRun, nil)
		}
	}
	if s.Has(EventAfterRun) {
		cfg.AfterRunCallback = func(ictx agent.InvocationContext) {
			_ = s.fire(ictx, EventAfterRun, nil)
		}
	}
	if s.Has(EventOnUserMessage) {
		cfg.OnUserMessageCallback = func(ictx agent.InvocationContext, content *genai.Content) (*genai.Content, error) {
			return nil, s.fire(ictx, EventOnUserMessage, func(p *Payload) {
				fillContent(p, content)
			})
		}
	}
	if s.Has(EventBeforeAgent) {
		cfg.BeforeAgentCallback = func(ctx agent.Context) (*genai.Content, error) {
			return nil, s.fire(ctx, EventBeforeAgent, nil)
		}
	}
	if s.Has(EventAfterAgent) {
		cfg.AfterAgentCallback = func(ctx agent.Context) (*genai.Content, error) {
			return nil, s.fire(ctx, EventAfterAgent, nil)
		}
	}
	if s.Has(EventBeforeModel) {
		cfg.BeforeModelCallback = func(ctx agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
			return nil, s.fire(ctx, EventBeforeModel, func(p *Payload) {
				if req != nil {
					p.Model = req.Model
				}
			})
		}
	}
	if s.Has(EventAfterModel) {
		cfg.AfterModelCallback = func(ctx agent.Context, resp *model.LLMResponse, llmErr error) (*model.LLMResponse, error) {
			// Fire only on the final (non-partial) response of a generation.
			// The runner invokes this callback once per streamed chunk, and a
			// per-delta hook would spawn a shell per token — the same rule as
			// on_event ("Only completed events fire"). before_model fires once
			// per request, so after_model mirrors it once per generation, with
			// the full text and finish_reason in the payload.
			if resp != nil && resp.Partial {
				return nil, nil
			}
			_ = s.fire(ctx, EventAfterModel, func(p *Payload) {
				if resp == nil {
					return
				}
				p.FinishReason = string(resp.FinishReason)
				if llmErr == nil {
					fillContent(p, resp.Content)
				}
			})
			return nil, nil
		}
	}
	if s.Has(EventOnModelError) {
		cfg.OnModelErrorCallback = func(ctx agent.Context, req *model.LLMRequest, err error) (*model.LLMResponse, error) {
			_ = s.fire(ctx, EventOnModelError, func(p *Payload) {
				if req != nil {
					p.Model = req.Model
				}
				if err != nil {
					p.Error = clip(err.Error(), maxRunes)
				}
			})
			return nil, nil
		}
	}
	if s.Has(EventBeforeTool) {
		cfg.BeforeToolCallback = func(ctx agent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
			return nil, s.fire(ctx, EventBeforeTool, func(p *Payload) {
				p.Tool = t.Name()
				fillArgs(p, args)
			})
		}
	}
	if s.Has(EventAfterTool) {
		cfg.AfterToolCallback = func(ctx agent.Context, t tool.Tool, args, result map[string]any, err error) (map[string]any, error) {
			_ = s.fire(ctx, EventAfterTool, func(p *Payload) {
				p.Tool = t.Name()
				fillArgs(p, args)
				if err != nil {
					p.Error = clip(err.Error(), maxRunes)
				}
			})
			return nil, nil
		}
	}
	if s.Has(EventOnToolError) {
		cfg.OnToolErrorCallback = func(ctx agent.Context, t tool.Tool, args map[string]any, err error) (map[string]any, error) {
			_ = s.fire(ctx, EventOnToolError, func(p *Payload) {
				p.Tool = t.Name()
				fillArgs(p, args)
				if err != nil {
					p.Error = clip(err.Error(), maxRunes)
				}
			})
			return nil, nil
		}
	}
	if s.Has(EventOnEvent) {
		cfg.OnEventCallback = func(ictx agent.InvocationContext, ev *session.Event) (*session.Event, error) {
			// Only completed events fire on_event; a per-delta hook would run
			// once per streamed token.
			if ev == nil || ev.LLMResponse.Partial {
				return ev, nil
			}
			_ = s.fire(ictx, EventOnEvent, func(p *Payload) {
				p.EventID = ev.ID
				p.Author = ev.Author
				p.Final = ev.IsFinalResponse()
				fillContent(p, ev.Content)
				if p.Text == "" {
					p.Tool = toolNames(ev.Content)
				}
			})
			return ev, nil
		}
	}

	return plugin.New(cfg)
}

// fire builds a base payload from the invocation context and runs the hooks
// configured for event. It returns an error only when a blocking hook failed.
func (s *Set) fire(ictx agent.InvocationContext, event string, mutate func(*Payload)) error {
	if !s.Has(event) {
		return nil
	}
	p := &Payload{Event: event}
	fillContext(p, ictx)
	if mutate != nil {
		mutate(p)
	}
	// The ADK invocation context embeds a context.Context, so pending hooks
	// inherit its cancellation (e.g. Esc during streaming).
	return s.RunHooks(ictx, p)
}

// toolNames lists the function call/response names in a content, if any.
func toolNames(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var names []string
	for _, part := range c.Parts {
		switch {
		case part.FunctionCall != nil:
			names = append(names, part.FunctionCall.Name)
		case part.FunctionResponse != nil:
			names = append(names, part.FunctionResponse.Name)
		}
	}
	return strings.Join(names, ",")
}
