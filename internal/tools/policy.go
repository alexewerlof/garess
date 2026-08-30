package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Approval describes the policy decision for a requested action.
type Approval int

const (
	ApprovalAllow Approval = iota + 1
	ApprovalAsk
	ApprovalDeny
)

func (a Approval) String() string {
	switch a {
	case ApprovalAllow:
		return "allow"
	case ApprovalAsk:
		return "ask"
	case ApprovalDeny:
		return "deny"
	default:
		return "unknown"
	}
}

// Policy stores regexes that determine whether an operation is allowed, needs
// confirmation, or must be blocked. Precedence is deny > ask > allow.
type Policy struct {
	Allow []string
	Ask   []string
	Deny  []string
}

// DefaultPolicy loads policy rules from environment variables.
func DefaultPolicy() Policy {
	return Policy{
		Allow: splitEnv("GARESS_TOOL_ALLOW"),
		Ask:   splitEnv("GARESS_TOOL_ASK"),
		Deny:  splitEnv("GARESS_TOOL_DENY"),
	}
}

func splitEnv(name string) []string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ";")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Decision resolves a request string against the policy.
func (p Policy) Decision(target string) Approval {
	for _, pattern := range p.Deny {
		if ok, err := regexp.MatchString(pattern, target); err == nil && ok {
			return ApprovalDeny
		}
	}
	for _, pattern := range p.Ask {
		if ok, err := regexp.MatchString(pattern, target); err == nil && ok {
			return ApprovalAsk
		}
	}
	for _, pattern := range p.Allow {
		if ok, err := regexp.MatchString(pattern, target); err == nil && ok {
			return ApprovalAllow
		}
	}
	if len(p.Allow) == 0 && len(p.Ask) == 0 && len(p.Deny) == 0 {
		return ApprovalAllow
	}
	return ApprovalDeny
}

// DecisionFor resolves a tool call (name + JSON-ish args) against the policy.
func (p Policy) DecisionFor(name string, args map[string]any) Approval {
	return p.Decision(Target(name, args))
}

// Target builds the policy match string for a tool call: `name k1=v1 k2=v2`
// with keys sorted, mirroring the legacy wire-protocol target so existing
// GARESS_TOOL_* regexes keep matching.
func Target(name string, args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+1)
	parts = append(parts, name)
	for _, k := range keys {
		parts = append(parts, k+"="+renderValue(args[k]))
	}
	return strings.Join(parts, " ")
}

func renderValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool, float64, float32, int, int64, int32:
		return fmt.Sprint(t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

// ToolCall describes a single tool invocation (for display/logging).
type ToolCall struct {
	Name string
	Args map[string]any
}

// Event records a finished tool execution (for display/logging/hooks).
type Event struct {
	Call     ToolCall
	Approval Approval
	Output   string
	Error    string
}
