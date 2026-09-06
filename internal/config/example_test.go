package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestExampleLoadsAndValidates guards the bundled example-config.toml against
// drift: it must be non-empty, look like a config template, and — most
// importantly — load and validate exactly like a user config (defaults +
// merge + applyDefaults + Validate), so a change to the example that would
// break `garess init` is caught here.
func TestExampleLoadsAndValidates(t *testing.T) {
	tmpl := Example()
	if len(tmpl) == 0 {
		t.Fatal("Example() is empty — is example-config.toml embedded?")
	}
	if !strings.Contains(string(tmpl), "[[providers]]") {
		t.Fatal("Example() does not look like a config template (missing [[providers]])")
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	write(t, path, string(tmpl))
	cfg, found, err := loadFrom(path, "")
	if err != nil {
		t.Fatalf("bundled example-config.toml must load and validate cleanly: %v", err)
	}
	if !found {
		t.Fatal("bundled example-config.toml was not loaded")
	}
	if len(cfg.Providers) == 0 {
		t.Fatal("bundled example-config.toml must define at least one provider")
	}
	if cfg.Warnings != nil {
		t.Fatalf("bundled example-config.toml produced unknown-key warnings: %v", cfg.Warnings)
	}
}
