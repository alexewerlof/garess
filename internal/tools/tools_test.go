package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
)

func TestPolicyDecisionPrecedence(t *testing.T) {
	policy := Policy{
		Allow: []string{"^read_.*"},
		Ask:   []string{".*secret.*"},
		Deny:  []string{".*rm -rf.*"},
	}
	if got := policy.Decision("bash rm -rf /tmp"); got != ApprovalDeny {
		t.Fatalf("deny precedence failed: got %s want %s", got, ApprovalDeny)
	}
	if got := policy.Decision("bash echo secret"); got != ApprovalAsk {
		t.Fatalf("ask precedence failed: got %s want %s", got, ApprovalAsk)
	}
	if got := policy.Decision("read_file /tmp/file.txt"); got != ApprovalAllow {
		t.Fatalf("allow precedence failed: got %s want %s", got, ApprovalAllow)
	}
}

func TestDefaultPolicyAllowsWhenUnset(t *testing.T) {
	if got := (Policy{}).Decision("read_file /tmp/file"); got != ApprovalAllow {
		t.Fatalf("empty policy should allow by default, got %s", got)
	}
}

func TestDecisionForAndTarget(t *testing.T) {
	policy := Policy{
		Allow: []string{"^read_file "},
		Ask:   []string{".*secret.*"},
		Deny:  []string{"bash command=rm -rf.*"},
	}
	if got := policy.DecisionFor("bash", map[string]any{"command": "rm -rf /"}); got != ApprovalDeny {
		t.Errorf("deny via DecisionFor: got %s", got)
	}
	if got := policy.DecisionFor("bash", map[string]any{"command": "cat secret.txt"}); got != ApprovalAsk {
		t.Errorf("ask via DecisionFor: got %s", got)
	}
	if got := policy.DecisionFor("read_file", map[string]any{"path": "/tmp/x"}); got != ApprovalAllow {
		t.Errorf("allow via DecisionFor: got %s", got)
	}
	// Non-empty policy defaults to deny for unmatched calls.
	if got := policy.DecisionFor("bash", map[string]any{"command": "ls"}); got != ApprovalDeny {
		t.Errorf("default-deny for unmatched call: got %s", got)
	}

	if got := Target("bash", map[string]any{"command": "ls", "z": 1}); got != "bash command=ls z=1" {
		t.Errorf("Target = %q, want sorted key=value pairs", got)
	}
}

func TestBuildToolsNames(t *testing.T) {
	tools, err := BuildTools(nil, "", Policy{})
	if err != nil {
		t.Fatalf("BuildTools: %v", err)
	}
	want := BuiltinNames()
	if len(tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(tools), len(want))
	}
	for i, w := range want {
		if tools[i].Name() != w {
			t.Errorf("tools[%d].Name() = %q, want %q", i, tools[i].Name(), w)
		}
		if tools[i].Description() == "" {
			t.Errorf("tool %q has empty description", w)
		}
	}
}

// fakeTool is a minimal tool.Tool for callback tests.
type fakeTool struct {
	name string
}

func (f fakeTool) Name() string        { return f.name }
func (f fakeTool) Description() string { return "fake" }
func (f fakeTool) IsLongRunning() bool { return false }

func TestDenyCallback(t *testing.T) {
	// Non-empty policies default to deny, so an explicit allow rule is needed
	// for the non-denied call to pass.
	policy := Policy{Allow: []string{"^bash "}, Deny: []string{"bash command=rm -rf.*"}}
	cb := DenyCallback(policy)
	ctx := agent.NewStrictContextMock(context.Background())
	ctxp := &ctx

	if _, err := cb(ctxp, fakeTool{name: "bash"}, map[string]any{"command": "rm -rf /"}); err == nil {
		t.Fatal("denied call should return an error")
	} else if !strings.Contains(err.Error(), "denied by policy") {
		t.Errorf("error = %v", err)
	}
	if _, err := cb(ctxp, fakeTool{name: "bash"}, map[string]any{"command": "ls"}); err != nil {
		t.Errorf("allowed call should pass: %v", err)
	}
}

func TestCapOutput(t *testing.T) {
	if got := capOutput("small"); got != "small" {
		t.Errorf("small output should pass through, got %q", got)
	}
	big := strings.Repeat("x", MaxToolOutputChars+500)
	got := capOutput(big)
	if len(got) >= len(big) {
		t.Fatal("oversized output should be truncated")
	}
	if !strings.Contains(got, "truncated") {
		t.Error("truncation marker missing")
	}
	// Exactly at the cap: untouched.
	at := strings.Repeat("x", MaxToolOutputChars)
	if capOutput(at) != at {
		t.Error("output at the cap should not be truncated")
	}
}

func TestReadFileCapsLargeContent(t *testing.T) {
	ctx := agent.NewStrictContextMock(context.Background())
	b := &builder{mem: nil, workDir: t.TempDir(), policy: Policy{}}
	dir := t.TempDir()
	p := filepath.Join(dir, "big.txt")
	content := strings.Repeat("line\n", MaxToolOutputChars/4)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := b.readFile(&ctx, ReadFileInput{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	out, ok := res["output"].(string)
	if !ok {
		t.Fatalf("result = %#v", res)
	}
	if len(out) >= len(content) {
		t.Fatal("read_file should cap oversized file content")
	}
	if !strings.Contains(out, "truncated") {
		t.Error("truncation marker missing from read_file output")
	}
}

func TestBashCapsLargeOutput(t *testing.T) {
	ctx := agent.NewStrictContextMock(context.Background())
	b := &builder{mem: nil, workDir: t.TempDir(), policy: Policy{}}
	// 100k chars of output — comfortably over the cap.
	res, err := b.bash(&ctx, BashInput{Command: "head -c 100000 /dev/zero | tr '\\0' 'x'"})
	if err != nil {
		t.Fatal(err)
	}
	out, ok := res["output"].(string)
	if !ok {
		t.Fatalf("result = %#v", res)
	}
	if len(out) > MaxToolOutputChars+200 {
		t.Fatalf("bash output not capped: %d chars", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Error("truncation marker missing from bash output")
	}
}

func TestAskProviderViaDecisionFor(t *testing.T) {
	// askProvider is bound per-tool; verify the decision it encodes by
	// rebuilding the same logic through the exported helpers. A policy with
	// only ask rules still default-denies unmatched calls.
	policy := Policy{Ask: []string{".*secret.*"}}
	if policy.DecisionFor("bash", map[string]any{"command": "cat secret"}) != ApprovalAsk {
		t.Fatal("expected ask decision")
	}
	if policy.DecisionFor("bash", map[string]any{"command": "ls"}) != ApprovalDeny {
		t.Fatal("expected default-deny for unmatched call")
	}
}
