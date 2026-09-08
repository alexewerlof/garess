package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverProjectSkills(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "skills", "docs", "SKILL.md"), "document things\n")
	sources, err := Discover(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1: %+v", len(sources), sources)
	}
	if sources[0].Scope != "project" || sources[0].Name != "docs" {
		t.Fatalf("unexpected source: %+v", sources[0])
	}
}

func TestBuildIncludesUserAndProjectSkills(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, ".config"))
	write(t, filepath.Join(base, ".config", "garess", "skills", "setup", "SKILL.md"), "setup tasks\n")
	write(t, filepath.Join(base, "skills", "docs", "README.md"), "document tasks\n")
	text, err := Build(base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "setup tasks") || !strings.Contains(text, "document tasks") {
		t.Fatalf("skill content missing from build output:\n%s", text)
	}
	if !strings.Contains(text, "## Skill (setup") || !strings.Contains(text, "## Skill (docs") {
		t.Fatalf("expected skill headers in build output:\n%s", text)
	}
}

func TestRenderSkillImportsAndVars(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "skills", "lint")
	write(t, filepath.Join(dir, "SKILL.md"), "run {{TOOL}}:\n@rules.md\n")
	write(t, filepath.Join(dir, "rules.md"), "use tabs\n")
	t.Setenv("TOOL", "ruff")
	text, err := Render(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "run ruff:") || !strings.Contains(text, "use tabs") {
		t.Fatalf("skill render did not expand imports/vars:\n%s", text)
	}
}

// TestParseSkillFrontmatter verifies that skill frontmatter is parsed and
// filtered: the description is kept, unknown keys and the raw YAML block are
// stripped, and never reach the rendered body.
func TestParseSkillFrontmatter(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "skills", "web", "SKILL.md"), `---
name: web-research
description: Research web pages and summarize them.
allowed-tools: [fetch]
random-key: { nested: [yaml, that] }
---
Search the web and summarize.
`)
	sk, err := Parse(filepath.Join(dir, "skills", "web", "SKILL.md"), "project")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sk.Name != "web" {
		t.Errorf("Name = %q, want directory name web", sk.Name)
	}
	if sk.Description != "Research web pages and summarize them." {
		t.Errorf("Description = %q", sk.Description)
	}
	if sk.Body != "Search the web and summarize." {
		t.Errorf("Body = %q", sk.Body)
	}
	if strings.Contains(sk.Body, "---") || strings.Contains(sk.Body, "random-key") ||
		strings.Contains(sk.Body, "allowed-tools") {
		t.Errorf("frontmatter leaked into body:\n%s", sk.Body)
	}
}

// TestBuildSkillDescriptionIncluded verifies Build keeps the frontmatter
// description (a needed field) but not the raw YAML, when both a description
// and unknown keys are present.
func TestBuildSkillDescriptionIncluded(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "skills", "docs", "SKILL.md"), `---
description: Write and structure documentation.
whatever: [x, y]
---
Use clear headings.
`)
	text, err := Build(base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Write and structure documentation.") {
		t.Errorf("description missing from build:\n%s", text)
	}
	if !strings.Contains(text, "Use clear headings.") {
		t.Errorf("body missing from build:\n%s", text)
	}
	if strings.Contains(text, "whatever") || strings.Contains(text, "---") {
		t.Errorf("raw frontmatter leaked into build:\n%s", text)
	}
}

// TestDiscoverSkipsMalformedFrontmatter verifies one broken skill file does
// not fail or poison discovery.
func TestDiscoverSkipsMalformedFrontmatter(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "skills", "good", "SKILL.md"), "fine skill\n")
	write(t, filepath.Join(base, "skills", "bad", "SKILL.md"), "---\nname: [unclosed\n---\nbroken\n")
	skills, err := Discover(base)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(skills) != 1 || skills[0].Name != "good" {
		t.Errorf("got %d skills %+v, want only the good one", len(skills), skills)
	}
}
