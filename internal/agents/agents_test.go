package agents

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

func TestDiscoverProjectFile(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	sub := filepath.Join(repo, "cmd", "app")
	write(t, filepath.Join(repo, "AGENTS.md"), "root\n")
	write(t, filepath.Join(sub, "AGENTS.md"), "app\n")

	// from the launch dir (sub), only sub/AGENTS.md applies — the search is
	// capped there and does not climb to the repo root
	sources, err := Discover(sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1 (no parent files): %+v", len(sources), sources)
	}
	if sources[0].Scope != "project" || !strings.HasSuffix(sources[0].Path, filepath.Join(sub, "AGENTS.md")) {
		t.Fatalf("unexpected source: %+v", sources[0])
	}

	// launching from the repo root picks up the root file instead
	rootSources, err := Discover(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(rootSources) != 1 || !strings.HasSuffix(rootSources[0].Path, filepath.Join(repo, "AGENTS.md")) {
		t.Fatalf("unexpected sources from repo root: %+v", rootSources)
	}
}

func TestDiscoverUserFile(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, ".config"))
	userPath := filepath.Join(base, ".config", "agents", "AGENTS.md")
	write(t, userPath, "be polite\n")

	repo := filepath.Join(base, "repo")
	write(t, filepath.Join(repo, "AGENTS.md"), "root\n")

	sources, err := Discover(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("got %d sources, want 2 (user + project): %+v", len(sources), sources)
	}
	if sources[0].Scope != "user" || sources[0].Path != userPath {
		t.Fatalf("expected user file first, got %+v", sources[0])
	}
	if sources[1].Scope != "project" {
		t.Fatalf("expected project file second, got %+v", sources[1])
	}
}

func TestDiscoverNoFiles(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, ".config"))
	sources, err := Discover(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 0 {
		t.Fatalf("got %d sources, want 0", len(sources))
	}
}

func TestDiscoverSystemFiles(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, ".config"))
	// user-level SYSTEM.md lives in garess's own config dir, like pi.dev's
	// ~/.pi/agent/SYSTEM.md
	write(t, filepath.Join(base, ".config", "garess", "SYSTEM.md"), "global system\n")

	repo := filepath.Join(base, "repo")
	write(t, filepath.Join(repo, "AGENTS.md"), "root\n")
	write(t, filepath.Join(repo, "SYSTEM.md"), "system\n")

	sources, err := Discover(repo)
	if err != nil {
		t.Fatal(err)
	}
	// user SYSTEM.md + project AGENTS.md + project SYSTEM.md = 3 sources
	if len(sources) != 3 {
		t.Fatalf("got %d sources, want 3: %+v", len(sources), sources)
	}
	if sources[0].Scope != "user" || !strings.HasSuffix(sources[0].Path, filepath.Join(".config", "garess", "SYSTEM.md")) {
		t.Fatalf("expected user SYSTEM.md first, got %+v", sources[0])
	}
	if sources[1].Scope != "project" || !strings.HasSuffix(sources[1].Path, "AGENTS.md") {
		t.Fatalf("expected project AGENTS.md second, got %+v", sources[1])
	}
	if sources[2].Scope != "project" || !strings.HasSuffix(sources[2].Path, "SYSTEM.md") {
		t.Fatalf("expected project SYSTEM.md last, got %+v", sources[2])
	}
}

func TestBuildOrderingAndHeaders(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, ".config"))
	write(t, filepath.Join(base, ".config", "agents", "AGENTS.md"), "user rules\n")
	sub := filepath.Join(base, "repo", "sub")
	write(t, filepath.Join(sub, "AGENTS.md"), "sub rules\n")

	text, err := Build(sub)
	if err != nil {
		t.Fatal(err)
	}
	// user rules must appear before the launch-directory rules
	ui := strings.Index(text, "user rules")
	si := strings.Index(text, "sub rules")
	if ui < 0 || si < 0 {
		t.Fatalf("missing section in build output:\n%s", text)
	}
	if !(ui < si) {
		t.Fatalf("wrong ordering (user=%d sub=%d)", ui, si)
	}
	// headers name each file
	if !strings.Contains(text, "## AGENTS.md (user") || !strings.Contains(text, "## AGENTS.md (project") {
		t.Fatalf("missing headers:\n%s", text)
	}
}

func TestBuildSystemOrderingAndHeaders(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, ".config"))
	write(t, filepath.Join(base, ".config", "agents", "AGENTS.md"), "user agents\n")
	write(t, filepath.Join(base, ".config", "garess", "SYSTEM.md"), "user system\n")
	repo := filepath.Join(base, "repo")
	write(t, filepath.Join(repo, "AGENTS.md"), "project agents\n")
	write(t, filepath.Join(repo, "SYSTEM.md"), "project system\n")

	text, err := Build(repo)
	if err != nil {
		t.Fatal(err)
	}
	// order: user AGENTS.md, user SYSTEM.md, project AGENTS.md, project SYSTEM.md
	want := []string{"user agents", "user system", "project agents", "project system"}
	last := -1
	for _, w := range want {
		i := strings.Index(text, w)
		if i < 0 {
			t.Fatalf("missing %q in build output:\n%s", w, text)
		}
		if i < last {
			t.Fatalf("wrong ordering for %q", w)
		}
		last = i
	}
	// headers name each file with its basename
	if !strings.Contains(text, "## AGENTS.md (user") ||
		!strings.Contains(text, "## SYSTEM.md (user") ||
		!strings.Contains(text, "## AGENTS.md (project") ||
		!strings.Contains(text, "## SYSTEM.md (project") {
		t.Fatalf("missing headers:\n%s", text)
	}
}

func TestBuildSystemOnly(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	write(t, filepath.Join(repo, "SYSTEM.md"), "only a system file\n")

	text, err := Build(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "## SYSTEM.md (project") {
		t.Fatalf("missing SYSTEM.md header:\n%s", text)
	}
	if !strings.Contains(text, "only a system file") {
		t.Fatalf("missing SYSTEM.md content:\n%s", text)
	}
}

func TestRenderImportsAndVars(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "proj")
	write(t, filepath.Join(dir, "AGENTS.md"), "Use the style below:\n@style.md\n{{EDITOR}} handles edits.\n")
	write(t, filepath.Join(dir, "style.md"), "tabs not spaces\n")
	t.Setenv("EDITOR", "vim")

	text, err := Render(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "tabs not spaces") {
		t.Fatalf("import not expanded:\n%s", text)
	}
	if !strings.Contains(text, "vim handles edits") {
		t.Fatalf("variable not expanded:\n%s", text)
	}
	if strings.Contains(text, "@style.md") {
		t.Fatalf("import line not removed:\n%s", text)
	}
}

func TestRenderImportCycle(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "a.md"), "@b.md\n")
	write(t, filepath.Join(base, "b.md"), "@a.md\n")
	if _, err := Render(filepath.Join(base, "a.md")); err == nil {
		t.Fatal("expected an error for an import cycle")
	}
}

func TestRenderMissingImport(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "a.md"), "@nope.md\n")
	if _, err := Render(filepath.Join(base, "a.md")); err == nil {
		t.Fatal("expected an error for a missing import")
	}
}
