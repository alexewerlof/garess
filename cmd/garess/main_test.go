package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"garess/internal/config"
)

// chdirTemp moves the process into a fresh temp dir for the test and restores
// the original working directory when it finishes. Do not use t.Parallel in
// tests that call it: the working directory is process-global.
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(old)
	})
	return dir
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// it wrote (printUsage and friends print via fmt.Print to os.Stdout).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	return string(out)
}

func TestHelpFlags(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"-help"}, {"--help"}, {"help"}} {
		out := captureStdout(t, func() {
			if err := run(args); err != nil {
				t.Errorf("run(%v): %v", args, err)
			}
		})
		if !strings.Contains(out, "Usage:") || !strings.Contains(out, "garess init") {
			t.Errorf("run(%v) output missing usage text:\n%s", args, out)
		}
	}
}

func TestSubcommandHelp(t *testing.T) {
	for _, args := range [][]string{{"init", "-h"}, {"doctor", "-h"}} {
		out := captureStdout(t, func() {
			if err := run(args); err != nil {
				t.Errorf("run(%v): %v", args, err)
			}
		})
		if !strings.Contains(out, "Usage:") {
			t.Errorf("run(%v) should print usage, got:\n%s", args, out)
		}
	}
}

func TestInitWritesLocalConfig(t *testing.T) {
	dir := chdirTemp(t)
	if err := run([]string{"init"}); err != nil {
		t.Fatalf("run init: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("init did not write ./config.toml: %v", err)
	}
	if !bytes.Equal(got, config.Example()) {
		t.Errorf("written config differs from the bundled example (got %d bytes, want %d)", len(got), len(config.Example()))
	}
}

func TestInitRefusesOverwrite(t *testing.T) {
	chdirTemp(t)
	path := filepath.Join(".", "config.toml")
	if err := os.WriteFile(path, []byte("# my config\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := run([]string{"init"})
	if err == nil {
		t.Fatal("init should refuse to overwrite an existing config")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("refusal should mention the existing file, got: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "# my config\n" {
		t.Errorf("existing config was clobbered without -f: %q", got)
	}

	if err := run([]string{"init", "-f"}); err != nil {
		t.Fatalf("run init -f: %v", err)
	}
	got, _ = os.ReadFile(path)
	if !bytes.Equal(got, config.Example()) {
		t.Error("init -f did not overwrite with the bundled example")
	}
}

func TestInitGlobal(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	chdirTemp(t)

	if err := run([]string{"init", "-g"}); err != nil {
		t.Fatalf("run init -g: %v", err)
	}
	path := filepath.Join(cfgHome, "garess", "config.toml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("init -g did not write the global config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("global config mode = %o, want 600", perm)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, config.Example()) {
		t.Error("global config differs from the bundled example")
	}
}

func TestInitRejectsStrayArgs(t *testing.T) {
	chdirTemp(t)
	err := run([]string{"init", "extra"})
	if err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("init should reject positional args, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(".", "config.toml")); statErr == nil {
		t.Error("init with stray args must not write a config")
	}
}

// writeFile creates path with content, creating parents as needed.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReportPersonas(t *testing.T) {
	dir := chdirTemp(t)
	// Project personas: .garess/agents/ and plain agents/.
	writeFile(t, filepath.Join(dir, ".garess", "agents", "researcher.md"), `---
name: researcher
description: Reads code.
tools: [read_file, grep]
---
Research the code.
`)
	writeFile(t, filepath.Join(dir, "agents", "reviewer.md"), `---
description: Reviews changes.
---
Review.
`)
	// A user-level persona that is not model-invocable.
	writeFile(t, filepath.Join(dir, ".config", "garess", "agents", "internal.md"), `---
name: internal
description: Hidden helper.
disable-model-invocation: true
---
Helper.
`)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))

	out := captureStdout(t, reportPersonas)
	for _, want := range []string{"personas:", "researcher: Reads code.", "reviewer: Reviews changes.", "internal: Hidden helper.", "not model-invocable"} {
		if !strings.Contains(out, want) {
			t.Errorf("reportPersonas missing %q:\n%s", want, out)
		}
	}
}

func TestReportPersonasNone(t *testing.T) {
	chdirTemp(t)
	out := captureStdout(t, reportPersonas)
	if !strings.Contains(out, "no custom-agent files") {
		t.Errorf("expected the no-personas message, got:\n%s", out)
	}
}
