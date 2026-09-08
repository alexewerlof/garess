package personas

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseFrontmatterVariants(t *testing.T) {
	dir := t.TempDir()

	// tools as a YAML list, name from frontmatter.
	writeFile(t, filepath.Join(dir, "reviewer.md"), `---
name: security-reviewer
description: Reviews code for security issues.
tools:
  - read_file
  - grep
---
Audit for vulnerabilities.
`)
	p, err := Parse(filepath.Join(dir, "reviewer.md"), ScopeProject)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Name != "security-reviewer" {
		t.Errorf("Name = %q, want security-reviewer", p.Name)
	}
	if p.Description != "Reviews code for security issues." {
		t.Errorf("Description = %q", p.Description)
	}
	if len(p.Tools) != 2 || p.Tools[0] != "read_file" || p.Tools[1] != "grep" {
		t.Errorf("Tools = %v", p.Tools)
	}
	if !p.UserInvocable {
		t.Error("UserInvocable should default to true")
	}
	if !p.ModelInvocable() {
		t.Error("ModelInvocable should default to true")
	}
	if p.Body != "Audit for vulnerabilities." {
		t.Errorf("Body = %q", p.Body)
	}
	if p.Scope != ScopeProject {
		t.Errorf("Scope = %q", p.Scope)
	}

	// tools as a comma string (Claude format), name defaults to file stem,
	// user-invocable false, disable-model-invocation true.
	writeFile(t, filepath.Join(dir, "internal-helper.md"), `---
description: Only usable as a subagent.
tools: "read_file, grep"
user-invocable: false
disable-model-invocation: true
---
Helper body.
`)
	p2, err := Parse(filepath.Join(dir, "internal-helper.md"), ScopeUser)
	if err != nil {
		t.Fatalf("Parse 2: %v", err)
	}
	if p2.Name != "internal-helper" {
		t.Errorf("Name = %q, want file stem", p2.Name)
	}
	if len(p2.Tools) != 2 || p2.Tools[0] != "read_file" {
		t.Errorf("Tools = %v", p2.Tools)
	}
	if p2.UserInvocable {
		t.Error("UserInvocable should be false")
	}
	if p2.ModelInvocable() {
		t.Error("ModelInvocable should be false with disable-model-invocation")
	}
}

func TestParseErrors(t *testing.T) {
	dir := t.TempDir()

	// No frontmatter at all.
	writeFile(t, filepath.Join(dir, "plain.md"), "Just prose, no frontmatter.\n")
	if _, err := Parse(filepath.Join(dir, "plain.md"), ScopeProject); err == nil {
		t.Error("expected error for a file without frontmatter")
	}

	// Malformed YAML.
	writeFile(t, filepath.Join(dir, "broken.md"), "---\nname: [unclosed\n---\nbody\n")
	if _, err := Parse(filepath.Join(dir, "broken.md"), ScopeProject); err == nil {
		t.Error("expected error for malformed YAML")
	}
}

func TestParseRendersBodyImportsAndVars(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PERSONA_TEST_VAR", "expanded")

	writeFile(t, filepath.Join(dir, "guidelines.md"), "Shared guidelines for {{PERSONA_TEST_VAR}} work.\n")
	writeFile(t, filepath.Join(dir, "planner.md"), `---
name: planner
---
Plan carefully.

@guidelines.md
`)
	p, err := Parse(filepath.Join(dir, "planner.md"), ScopeProject)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, want := range []string{"Plan carefully.", "Shared guidelines for expanded work."} {
		if !strings.Contains(p.Body, want) {
			t.Errorf("Body missing %q:\n%s", want, p.Body)
		}
	}
	if strings.Contains(p.Body, "@guidelines.md") {
		t.Error("@import line not removed from body")
	}
}

func TestValidateDropsUnknownTools(t *testing.T) {
	p := Persona{Name: "x", Tools: []string{"read_file", "no_such_tool", "run_subagent"}}
	p = p.Validate([]string{"bash", "read_file", "grep", "glob", "write_file", "memory_read", "memory_write", "memory_list", "memory_delete", "run_subagent"})
	if len(p.Tools) != 2 || p.Tools[0] != "read_file" || p.Tools[1] != "run_subagent" {
		t.Errorf("Tools = %v", p.Tools)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "no_such_tool") {
		t.Errorf("Warnings = %v", p.Warnings)
	}
}

func TestDiscoverRootsAndOverride(t *testing.T) {
	// User root under XDG_CONFIG_HOME, project roots under dir.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	userRoot := filepath.Join(xdg, "garess", "agents")
	writeFile(t, filepath.Join(userRoot, "shared.md"), "---\nname: shared\ndescription: user version\n---\nUser body.\n")
	writeFile(t, filepath.Join(userRoot, "user-only.md"), "---\nname: user-only\ndescription: Only at user scope\n---\nBody.\n")

	dir := t.TempDir()
	// .garess/agents project root overrides the user "shared".
	writeFile(t, filepath.Join(dir, ".garess", "agents", "shared.md"), "---\nname: shared\ndescription: project version\ntools: [read_file]\n---\nProject body.\n")
	// plain agents/ root adds a project-only persona; a README is skipped.
	writeFile(t, filepath.Join(dir, "agents", "coder.md"), "---\nname: coder\ndescription: Writes code\n---\nCode body.\n")
	writeFile(t, filepath.Join(dir, "agents", "README.md"), "# README\nnot a persona\n")

	got, err := Discover(dir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	byName := map[string]Persona{}
	for _, p := range got {
		byName[p.Name] = p
	}

	shared, ok := byName["shared"]
	if !ok {
		t.Fatalf("no shared persona; got %v", byName)
	}
	if shared.Scope != ScopeProject || shared.Description != "project version" || shared.Body != "Project body." {
		t.Errorf("shared not overridden by project: %+v", shared)
	}
	if _, ok := byName["user-only"]; !ok {
		t.Error("user-only persona missing")
	}
	if _, ok := byName["coder"]; !ok {
		t.Error("coder persona missing from plain agents/ root")
	}
	if _, ok := byName["README"]; ok {
		t.Error("README.md parsed as a persona")
	}
}
