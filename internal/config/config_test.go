package config

import (
	"errors"
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
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadExplicit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	write(t, p, `
default_provider = "local"

[[providers]]
name = "local"
endpoint = "http://127.0.0.1:8080/v1"
api_key = ""
model = "qwen2.5-0.5b"

[[providers]]
name = "remote"
endpoint = "https://api.example.com/v1"
api_key = "sk-secret"
model = "cool-model"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultProvider != "local" {
		t.Fatalf("default provider = %q", cfg.DefaultProvider)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("got %d providers", len(cfg.Providers))
	}
	// defaults applied
	if cfg.Session.HistoryLimit != DefaultHistoryLimit {
		t.Fatalf("history limit = %d", cfg.Session.HistoryLimit)
	}
	if cfg.TUI.Theme != DefaultTheme {
		t.Fatalf("theme = %q", cfg.TUI.Theme)
	}
	// named lookup works
	if p, ok := cfg.Provider("remote"); !ok || p.APIKey != "sk-secret" {
		t.Fatalf("provider lookup failed: %+v %v", p, ok)
	}
}

func TestValidateRequiresProviders(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	write(t, p, "")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for empty config")
	}
}

func TestValidateBadEndpoint(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	write(t, p, `
[[providers]]
name = "x"
endpoint = "not-a-url"
model = "m"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("expected endpoint error, got %v", err)
	}
}

func TestValidateUnknownTheme(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	write(t, p, `
[tui]
theme = "neon"

[[providers]]
name = "x"
endpoint = "https://x.example/v1"
model = "m"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "theme") {
		t.Fatalf("expected theme error, got %v", err)
	}
}

func TestMergeGlobalAndProject(t *testing.T) {
	dir := t.TempDir()
	global := filepath.Join(dir, "global.toml")
	project := filepath.Join(dir, "project.toml")

	write(t, global, `
default_provider = "a"
[[providers]]
name = "a"
endpoint = "https://a.example/v1"
model = "ma"
[[providers]]
name = "b"
endpoint = "https://b.example/v1"
model = "mb"
[session]
history_limit = 10
`)
	write(t, project, `
default_provider = "b"
[[providers]]
name = "b"
endpoint = "https://b-project.example/v1"
model = "mb-new"
[tui]
theme = "light"
`)

	cfg, found, err := loadFrom(global, project)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected config files to be found")
	}
	if cfg.DefaultProvider != "b" {
		t.Fatalf("default provider = %q", cfg.DefaultProvider)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("got %d providers, want 2", len(cfg.Providers))
	}
	pb, ok := cfg.Provider("b")
	if !ok {
		t.Fatal("provider b missing")
	}
	if pb.Endpoint != "https://b-project.example/v1" || pb.Model != "mb-new" {
		t.Fatalf("project should override b: %+v", pb)
	}
	pa, _ := cfg.Provider("a")
	if pa.Endpoint != "https://a.example/v1" {
		t.Fatalf("a should keep global values: %+v", pa)
	}
	if cfg.Session.HistoryLimit != 10 {
		t.Fatalf("history limit = %d, want 10 (from global)", cfg.Session.HistoryLimit)
	}
	if cfg.TUI.Theme != "light" {
		t.Fatalf("theme = %q, want light (from project)", cfg.TUI.Theme)
	}
}

func TestLoadNotFound(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		filepath.Join(dir, "global.toml"),
		filepath.Join(dir, "project.toml"),
		filepath.Join(dir, "root.toml"),
	}
	_, err := loadSearch(paths)
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected NotFoundError, got %v", err)
	}
	if len(nf.Searched) != 3 {
		t.Fatalf("expected 3 searched paths, got %d: %v", len(nf.Searched), nf.Searched)
	}
}

func TestLoadExplicitMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.toml")
	_, err := Load(path)
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected NotFoundError, got %v", err)
	}
	if len(nf.Searched) != 1 || nf.Searched[0] != path {
		t.Fatalf("expected the explicit path to be reported, got %v", nf.Searched)
	}
}

func TestLoadFoundButNoProviders(t *testing.T) {
	dir := t.TempDir()
	global := filepath.Join(dir, "global.toml")
	write(t, global, "# empty config\n")

	_, err := loadSearch([]string{global})
	var inv *InvalidConfigError
	if !errors.As(err, &inv) {
		t.Fatalf("expected InvalidConfigError, got %v", err)
	}
	if !errors.Is(inv.Err, ErrNoProviders) {
		t.Fatalf("expected ErrNoProviders, got %v", inv.Err)
	}
	if inv.Path != global {
		t.Fatalf("expected path %q, got %q", global, inv.Path)
	}
}

func TestLoadInvalidEndpoint(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	write(t, p, `
[[providers]]
name = "x"
endpoint = "not-a-url"
model = "m"
`)
	_, err := Load(p)
	var inv *InvalidConfigError
	if !errors.As(err, &inv) {
		t.Fatalf("expected InvalidConfigError, got %v", err)
	}
	if !strings.Contains(inv.Err.Error(), "endpoint") {
		t.Fatalf("expected endpoint error, got %v", inv.Err)
	}
	if inv.Path != p {
		t.Fatalf("expected path %q, got %q", p, inv.Path)
	}
}

func TestLoadProjectRootConfigFallback(t *testing.T) {
	dir := t.TempDir()
	global := filepath.Join(dir, "global.toml")   // missing
	project := filepath.Join(dir, ".garess.toml") // missing
	root := filepath.Join(dir, "root.toml")       // exists
	write(t, root, `
[[providers]]
name = "x"
endpoint = "https://x.example/v1"
model = "m"
`)

	cfg, err := loadSearch([]string{global, project, root})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultProvider != "x" {
		t.Fatalf("default provider = %q", cfg.DefaultProvider)
	}
}

func TestUnknownKeyWarning(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	write(t, p, `
[[providers]]
name = "x"
endpoint = "https://x.example/v1"
model = "m"
bogus_key = 1
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) == 0 || !strings.Contains(cfg.Warnings[0], "bogus_key") {
		t.Fatalf("expected warning about bogus_key, got %v", cfg.Warnings)
	}
}

func TestResolveAPIKeyEnv(t *testing.T) {
	p := &Provider{Name: "x", APIKey: "from-file"}
	if got := ResolveAPIKey(p); got != "from-file" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("GA_RESS_API_KEY", "from-env")
	if got := ResolveAPIKey(p); got != "from-env" {
		t.Fatalf("got %q", got)
	}
}
