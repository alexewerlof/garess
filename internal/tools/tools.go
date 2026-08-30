// Package tools provides the built-in harness functiontools for ADK agents:
// bash, file read/write, glob, grep, and memory note operations, gated by a
// regex allow/ask/deny policy. Tool calls use native OpenAI function calling
// (no text wire protocol).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"garess/internal/memory"
)

// BuiltinNames lists the tools BuildTools produces, in registration order.
func BuiltinNames() []string {
	return []string{
		"bash", "read_file", "write_file", "glob", "grep",
		"memory_read", "memory_write", "memory_list", "memory_delete",
	}
}

// BuildTools constructs the built-in functiontools for an ADK agent. The
// policy is bound at construction: calls matching an ask rule request HITL
// confirmation (RequireConfirmationProvider); calls matching a deny rule are
// blocked by DenyCallback.
func BuildTools(mem *memory.Store, workDir string, policy Policy) ([]tool.Tool, error) {
	b := &builder{mem: mem, workDir: workDir, policy: policy}
	return b.build()
}

// DenyCallback returns an llmagent.BeforeToolCallback that blocks tool calls
// denied by the policy. It runs before every tool, so deny wins over ask and
// allow regardless of RequireConfirmationProvider.
//
// NOTE: a non-nil (response, nil) return from a BeforeToolCallback short-
// circuits the tool and uses the response as the result, so allowed calls
// must return (nil, nil) to let the tool run.
func DenyCallback(policy Policy) llmagent.BeforeToolCallback {
	return func(ctx agent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
		if policy.DecisionFor(t.Name(), args) == ApprovalDeny {
			return nil, fmt.Errorf("tool %q denied by policy", t.Name())
		}
		return nil, nil // pass through: run the tool
	}
}

// askProvider builds the RequireConfirmationProvider for one tool: it asks for
// confirmation exactly when the policy decision for the call is ApprovalAsk.
func askProvider[TArgs any](policy Policy, name string) func(TArgs) bool {
	return func(in TArgs) bool {
		b, err := json.Marshal(in)
		if err != nil {
			return false
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return false
		}
		return policy.DecisionFor(name, m) == ApprovalAsk
	}
}

type builder struct {
	mem     *memory.Store
	workDir string
	policy  Policy
}

// toolConfig builds the functiontool.Config for one tool, binding the ask
// policy as its RequireConfirmationProvider.
func toolConfig[TArgs any](policy Policy, name, desc string) functiontool.Config {
	return functiontool.Config{
		Name:                        name,
		Description:                 desc,
		RequireConfirmationProvider: askProvider[TArgs](policy, name),
	}
}

func (b *builder) build() ([]tool.Tool, error) {
	var out []tool.Tool
	add := func(t tool.Tool, err error) error {
		if err != nil {
			return err
		}
		out = append(out, t)
		return nil
	}
	if err := add(functiontool.New(toolConfig[BashInput](b.policy, "bash", "Run a shell command with `bash -lc`. Returns combined stdout and stderr."), b.bash)); err != nil {
		return nil, err
	}
	if err := add(functiontool.New(toolConfig[ReadFileInput](b.policy, "read_file", "Read a file and return its contents."), b.readFile)); err != nil {
		return nil, err
	}
	if err := add(functiontool.New(toolConfig[WriteFileInput](b.policy, "write_file", "Write content to a file, creating parent directories as needed."), b.writeFile)); err != nil {
		return nil, err
	}
	if err := add(functiontool.New(toolConfig[GlobInput](b.policy, "glob", "Expand a glob pattern (e.g. **/*.go) to matching paths."), b.glob)); err != nil {
		return nil, err
	}
	if err := add(functiontool.New(toolConfig[GrepInput](b.policy, "grep", "Search a file or directory tree for lines matching a regular expression."), b.grep)); err != nil {
		return nil, err
	}
	if err := add(functiontool.New(toolConfig[MemoryReadInput](b.policy, "memory_read", "Read a local (project) or global memory note."), b.memoryRead)); err != nil {
		return nil, err
	}
	if err := add(functiontool.New(toolConfig[MemoryWriteInput](b.policy, "memory_write", "Save a memory note. Local scope by default; set global=true for the user-wide scope."), b.memoryWrite)); err != nil {
		return nil, err
	}
	if err := add(functiontool.New(toolConfig[MemoryListInput](b.policy, "memory_list", "List memory notes in the local (project) or global scope."), b.memoryList)); err != nil {
		return nil, err
	}
	if err := add(functiontool.New(toolConfig[MemoryDeleteInput](b.policy, "memory_delete", "Delete a memory note from the local or global scope."), b.memoryDelete)); err != nil {
		return nil, err
	}
	return out, nil
}

// output wraps a tool result for the model as a JSON object.
func output(v any) map[string]any {
	return map[string]any{"output": v}
}

// BashInput is the argument schema for the bash tool.
type BashInput struct {
	Command string `json:"command" jsonschema:"Shell command to run with bash -lc"`
}

func (b *builder) bash(ctx agent.Context, in BashInput) (map[string]any, error) {
	if strings.TrimSpace(in.Command) == "" {
		return nil, fmt.Errorf("bash requires a command argument")
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "bash", "-lc", in.Command)
	if b.workDir != "" {
		cmd.Dir = b.workDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("bash failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return output(strings.TrimSpace(string(out))), nil
}

// ReadFileInput is the argument schema for the read_file tool.
type ReadFileInput struct {
	Path string `json:"path" jsonschema:"Path of the file to read"`
}

func (b *builder) readFile(ctx agent.Context, in ReadFileInput) (map[string]any, error) {
	if in.Path == "" {
		return nil, fmt.Errorf("read_file requires a path")
	}
	data, err := os.ReadFile(in.Path)
	if err != nil {
		return nil, err
	}
	return output(string(data)), nil
}

// WriteFileInput is the argument schema for the write_file tool.
type WriteFileInput struct {
	Path    string `json:"path" jsonschema:"Path of the file to write"`
	Content string `json:"content" jsonschema:"Content to write to the file"`
}

func (b *builder) writeFile(ctx agent.Context, in WriteFileInput) (map[string]any, error) {
	if in.Path == "" {
		return nil, fmt.Errorf("write_file requires a path")
	}
	if err := os.MkdirAll(filepath.Dir(in.Path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(in.Path, []byte(in.Content), 0o644); err != nil {
		return nil, err
	}
	return output(fmt.Sprintf("wrote %s", in.Path)), nil
}

// GlobInput is the argument schema for the glob tool.
type GlobInput struct {
	Pattern string `json:"pattern" jsonschema:"Glob pattern, e.g. **/*.go"`
}

func (b *builder) glob(ctx agent.Context, in GlobInput) (map[string]any, error) {
	if in.Pattern == "" {
		return nil, fmt.Errorf("glob requires a pattern")
	}
	matches, err := filepath.Glob(in.Pattern)
	if err != nil {
		return nil, err
	}
	return output(strings.Join(matches, "\n")), nil
}

// GrepInput is the argument schema for the grep tool.
type GrepInput struct {
	Pattern string `json:"pattern" jsonschema:"Regular expression to search for"`
	Path    string `json:"path" jsonschema:"File or directory to search"`
}

func (b *builder) grep(ctx agent.Context, in GrepInput) (map[string]any, error) {
	if in.Pattern == "" || in.Path == "" {
		return nil, fmt.Errorf("grep requires pattern and path")
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return nil, err
	}
	var out []string
	walkErr := filepath.Walk(in.Path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		if re.Match(data) {
			out = append(out, p)
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return output(strings.Join(out, "\n")), nil
}

// MemoryReadInput is the argument schema for the memory_read tool.
type MemoryReadInput struct {
	Name string `json:"name" jsonschema:"Name of the memory note to read"`
}

func (b *builder) memoryRead(ctx agent.Context, in MemoryReadInput) (map[string]any, error) {
	if b.mem == nil {
		return nil, fmt.Errorf("memory store is not configured")
	}
	content, local, err := b.mem.Read(in.Name)
	if err != nil {
		return nil, err
	}
	if local {
		return output("local: " + content), nil
	}
	return output("global: " + content), nil
}

// MemoryWriteInput is the argument schema for the memory_write tool.
type MemoryWriteInput struct {
	Name    string `json:"name" jsonschema:"Name of the memory note to save"`
	Content string `json:"content" jsonschema:"Content of the note"`
	Global  bool   `json:"global" jsonschema:"Save in the user-wide global scope instead of the project scope"`
}

func (b *builder) memoryWrite(ctx agent.Context, in MemoryWriteInput) (map[string]any, error) {
	if b.mem == nil {
		return nil, fmt.Errorf("memory store is not configured")
	}
	if in.Name == "" || in.Content == "" {
		return nil, fmt.Errorf("memory_write requires name and content")
	}
	if err := b.mem.Write(in.Name, in.Content, in.Global); err != nil {
		return nil, err
	}
	return output(fmt.Sprintf("saved note %s", in.Name)), nil
}

// MemoryListInput is the argument schema for the memory_list tool.
type MemoryListInput struct {
	Global bool `json:"global" jsonschema:"List the user-wide global scope instead of the project scope"`
}

func (b *builder) memoryList(ctx agent.Context, in MemoryListInput) (map[string]any, error) {
	if b.mem == nil {
		return nil, fmt.Errorf("memory store is not configured")
	}
	notes, err := b.mem.List()
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, n := range notes {
		if in.Global != (n.Scope == "global") {
			continue
		}
		lines = append(lines, n.Name)
	}
	return output(strings.Join(lines, "\n")), nil
}

// MemoryDeleteInput is the argument schema for the memory_delete tool.
type MemoryDeleteInput struct {
	Name   string `json:"name" jsonschema:"Name of the memory note to delete"`
	Global bool   `json:"global" jsonschema:"Delete from the user-wide global scope instead of the project scope"`
}

func (b *builder) memoryDelete(ctx agent.Context, in MemoryDeleteInput) (map[string]any, error) {
	if b.mem == nil {
		return nil, fmt.Errorf("memory store is not configured")
	}
	if err := b.mem.Delete(in.Name, in.Global); err != nil {
		return nil, err
	}
	return output(fmt.Sprintf("deleted note %s", in.Name)), nil
}
