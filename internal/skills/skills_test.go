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
