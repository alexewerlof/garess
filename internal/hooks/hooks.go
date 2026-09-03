// Package hooks implements git-style shell hooks for garess agent events.
//
// Hooks are shell commands configured in config.toml ([[hooks]] entries) and
// run whenever the matching ADK event fires. A hook is executed with
// `sh -c <command>`; the event name is available as $1 (argv[1]) and as the
// GARESS_HOOK_EVENT environment variable, and a JSON payload describing the
// event is written to the hook's stdin. stdout/stderr are captured (they never
// touch the TUI) and surface in logs and in blocking error messages.
//
// Events are split into two groups:
//
//   - Blocking ("pre") hooks abort the operation when they exit non-zero:
//     before_run, on_user_message, before_agent, before_model, before_tool.
//   - Notification ("post"/"on") hooks never abort; a non-zero exit is logged
//     as a warning: after_run, after_agent, after_model, on_model_error,
//     after_tool, on_tool_error, on_event.
//
// Hooks are wired into the ADK runner via a plugin.Plugin built by NewPlugin;
// harness.Build registers it on runner.Config.PluginConfig when the config
// defines any hooks.
package hooks

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"garess/internal/config"
)

// Event names, one per ADK callback family that garess exposes as a hook.
const (
	EventBeforeRun     = "before_run"
	EventAfterRun      = "after_run"
	EventOnUserMessage = "on_user_message"
	EventBeforeAgent   = "before_agent"
	EventAfterAgent    = "after_agent"
	EventBeforeModel   = "before_model"
	EventAfterModel    = "after_model"
	EventOnModelError  = "on_model_error"
	EventBeforeTool    = "before_tool"
	EventAfterTool     = "after_tool"
	EventOnToolError   = "on_tool_error"
	EventOnEvent       = "on_event"
)

// ValidEvents lists every event name, in the order they are documented.
var ValidEvents = []string{
	EventBeforeRun,
	EventAfterRun,
	EventOnUserMessage,
	EventBeforeAgent,
	EventAfterAgent,
	EventBeforeModel,
	EventAfterModel,
	EventOnModelError,
	EventBeforeTool,
	EventAfterTool,
	EventOnToolError,
	EventOnEvent,
}

// blockingEvents are the events whose hooks gate an operation: a non-zero
// exit aborts it (skipping any remaining hooks for the event). All other
// events are notifications: failures are logged, never fatal.
var blockingEvents = map[string]bool{
	EventBeforeRun:     true,
	EventOnUserMessage: true,
	EventBeforeAgent:   true,
	EventBeforeModel:   true,
	EventBeforeTool:    true,
}

// IsValidEvent reports whether name is a known hook event.
func IsValidEvent(name string) bool {
	for _, e := range ValidEvents {
		if e == name {
			return true
		}
	}
	return false
}

// Blocking reports whether the named event aborts on a non-zero hook exit.
func Blocking(event string) bool { return blockingEvents[event] }

// DefaultTimeout bounds every hook run that does not set its own timeout.
const DefaultTimeout = 10 * time.Second

// Command is one parsed, validated hook.
type Command struct {
	Event   string
	Command string // shell command (sh -c)
	Timeout time.Duration
}

// Set holds parsed hook commands grouped by event. It is immutable after
// Parse, so a single Set can back many runners/plugins concurrently.
type Set struct {
	byEvent map[string][]Command
}

// Parse validates config hook definitions into a Set. Unknown events and
// unparseable or non-positive timeouts are errors.
func Parse(defs []config.Hook) (*Set, error) {
	s := &Set{byEvent: make(map[string][]Command)}
	for _, d := range defs {
		if d.Event == "" {
			return nil, errors.New("hook is missing an event (valid: " + strings.Join(ValidEvents, ", ") + ")")
		}
		if !IsValidEvent(d.Event) {
			return nil, fmt.Errorf("unknown hook event %q (valid: %s)", d.Event, strings.Join(ValidEvents, ", "))
		}
		if strings.TrimSpace(d.Command) == "" {
			return nil, fmt.Errorf("hook %q has an empty command", d.Event)
		}
		timeout := DefaultTimeout
		if d.Timeout != "" {
			t, err := time.ParseDuration(d.Timeout)
			if err != nil {
				return nil, fmt.Errorf("hook %q: invalid timeout %q: %v", d.Event, d.Timeout, err)
			}
			if t <= 0 {
				return nil, fmt.Errorf("hook %q: timeout %q must be positive", d.Event, d.Timeout)
			}
			timeout = t
		}
		s.byEvent[d.Event] = append(s.byEvent[d.Event], Command{
			Event:   d.Event,
			Command: d.Command,
			Timeout: timeout,
		})
	}
	return s, nil
}

// Has reports whether at least one hook is configured for the event.
func (s *Set) Has(event string) bool { return len(s.byEvent[event]) > 0 }
