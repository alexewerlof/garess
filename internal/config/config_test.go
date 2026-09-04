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

func TestSandboxDecodeDefaultsAndMerge(t *testing.T) {
	dir := t.TempDir()
	global := filepath.Join(dir, "global.toml")
	project := filepath.Join(dir, "project.toml")

	// Global: auto backend + one shared cache dir; project: landlock + own dirs.
	write(t, global, `
[[providers]]
name = "a"
endpoint = "https://a.example/v1"
model = "ma"
[sandbox]
backend = "auto"
write_dirs = ["/home/u/.cache"]
`)
	write(t, project, `
[[providers]]
name = "a"
endpoint = "https://a.example/v1"
model = "ma"
[sandbox]
backend = "landlock"
write_dirs = ["/home/u/.cache", "/var/tmp"]
`)

	cfg, found, err := loadFrom(global, project)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected config to be found")
	}
	if cfg.Sandbox.Backend != "landlock" {
		t.Fatalf("backend = %q, want landlock (project overrides)", cfg.Sandbox.Backend)
	}
	if len(cfg.Sandbox.WriteDirs) != 2 || cfg.Sandbox.WriteDirs[0] != "/home/u/.cache" || cfg.Sandbox.WriteDirs[1] != "/var/tmp" {
		t.Fatalf("write_dirs = %v, want the project list", cfg.Sandbox.WriteDirs)
	}

	// Defaults apply when the section is absent.
	cfg2 := Default()
	if cfg2.Sandbox.Backend != "none" {
		t.Fatalf("default backend = %q, want none", cfg2.Sandbox.Backend)
	}
}

func TestSandboxValidation(t *testing.T) {
	dir := t.TempDir()
	base := `
[[providers]]
name = "x"
endpoint = "https://x.example/v1"
model = "m"
`
	cases := []struct {
		name    string
		section string
		want    string
	}{
		{"bad backend", `[sandbox]
backend = "firejail"`, "sandbox.backend must be one of"},
		{"relative write dir", `[sandbox]
write_dirs = ["relative/path"]`, "must be an absolute path"},
		{"empty write dir", `[sandbox]
write_dirs = [""]`, "must not be empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(dir, "c.toml")
			write(t, p, base+c.section)
			_, err := Load(p)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
		})
	}

	// Valid section loads.
	p := filepath.Join(dir, "ok.toml")
	write(t, p, base+`[sandbox]
backend = "auto"
write_dirs = ["/tmp/x"]
`)
	if _, err := Load(p); err != nil {
		t.Fatalf("valid sandbox rejected: %v", err)
	}
}

func TestHooksDecode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	write(t, p, `
[[providers]]
name = "x"
endpoint = "https://x.example/v1"
model = "m"

[[hooks]]
event = "before_tool"
command = "guard.sh"

[[hooks]]
event = "on_event"
command = "notify --event"
timeout = "2s"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hooks) != 2 {
		t.Fatalf("got %d hooks, want 2: %+v", len(cfg.Hooks), cfg.Hooks)
	}
	if cfg.Hooks[0].Event != "before_tool" || cfg.Hooks[0].Command != "guard.sh" || cfg.Hooks[0].Timeout != "" {
		t.Fatalf("hook 0 = %+v", cfg.Hooks[0])
	}
	if cfg.Hooks[1].Event != "on_event" || cfg.Hooks[1].Command != "notify --event" || cfg.Hooks[1].Timeout != "2s" {
		t.Fatalf("hook 1 = %+v", cfg.Hooks[1])
	}
}

func TestMergeHooksProjectOverridesPerEvent(t *testing.T) {
	// Global defines two before_tool guards and an after_model notifier.
	global := []Hook{
		{Event: "before_tool", Command: "guard-a"},
		{Event: "before_tool", Command: "guard-b"},
		{Event: "after_model", Command: "notify-global"},
	}
	// Project redefines before_tool (drops the global guards) and adds on_event.
	project := []Hook{
		{Event: "before_tool", Command: "guard-project"},
		{Event: "on_event", Command: "audit"},
	}

	got := mergeHooks(global, project)
	wantEvents := []string{"after_model", "before_tool", "on_event"}
	if len(got) != len(wantEvents) {
		t.Fatalf("merged %d hooks, want %d: %+v", len(got), len(wantEvents), got)
	}
	for i, ev := range wantEvents {
		if got[i].Event != ev {
			t.Fatalf("hook %d event = %q, want %q (%+v)", i, got[i].Event, ev, got)
		}
	}
	// Project's before_tool replaces BOTH global guards.
	if got[1].Command != "guard-project" || got[1].Command == "guard-a" {
		t.Fatalf("before_tool should be the project hook, got %+v", got[1])
	}
	// The untouched global after_model hook survives.
	if got[0].Command != "notify-global" {
		t.Fatalf("after_model should keep the global hook, got %+v", got[0])
	}
	// Empty project hooks leave global hooks alone.
	if again := mergeHooks(global, nil); len(again) != len(global) {
		t.Fatalf("nil overlay changed hooks: %+v", again)
	}
}

func TestMCPDecode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	write(t, p, `
[[providers]]
name = "x"
endpoint = "https://x.example/v1"
model = "m"

[[mcp_servers]]
name = "scraper"
transport = "sse"
url = "http://scraper.local:9000/mcp/sse"
headers = { Authorization = "Bearer tok" }

[[mcp_servers]]
name = "local-fs"
transport = "stdio"
command = "/usr/bin/npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
env = { NODE_ENV = "production" }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 2 {
		t.Fatalf("got %d mcp_servers, want 2: %+v", len(cfg.MCPServers), cfg.MCPServers)
	}
	s := cfg.MCPServers[0]
	if s.Name != "scraper" || s.Transport != MCPTransportSSE || s.URL != "http://scraper.local:9000/mcp/sse" {
		t.Fatalf("server 0 = %+v", s)
	}
	if s.Headers["Authorization"] != "Bearer tok" {
		t.Fatalf("server 0 headers = %+v", s.Headers)
	}
	fs := cfg.MCPServers[1]
	if fs.Name != "local-fs" || fs.Transport != MCPTransportStdio || fs.Command != "/usr/bin/npx" {
		t.Fatalf("server 1 = %+v", fs)
	}
	if len(fs.Args) != 3 || fs.Args[0] != "-y" {
		t.Fatalf("server 1 args = %+v", fs.Args)
	}
	if fs.Env["NODE_ENV"] != "production" {
		t.Fatalf("server 1 env = %+v", fs.Env)
	}
}

func TestMergeMCPServersProjectOverridesByName(t *testing.T) {
	dir := t.TempDir()
	global := filepath.Join(dir, "global.toml")
	project := filepath.Join(dir, "project.toml")

	write(t, global, `
[[providers]]
name = "a"
endpoint = "https://a.example/v1"
model = "ma"
[[mcp_servers]]
name = "shared"
transport = "sse"
url = "http://global:9000/mcp/sse"
[[mcp_servers]]
name = "only-global"
transport = "stdio"
command = "tool-global"
`)
	write(t, project, `
[[providers]]
name = "a"
endpoint = "https://a.example/v1"
model = "ma"
[[mcp_servers]]
name = "shared"
transport = "sse"
url = "http://project:9000/mcp/sse"
`)

	cfg, found, err := loadFrom(global, project)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected config files to be found")
	}
	if len(cfg.MCPServers) != 2 {
		t.Fatalf("got %d mcp_servers, want 2 (only-global survives, shared replaced): %+v", len(cfg.MCPServers), cfg.MCPServers)
	}
	byName := map[string]MCPServer{}
	for _, s := range cfg.MCPServers {
		byName[s.Name] = s
	}
	if byName["shared"].URL != "http://project:9000/mcp/sse" {
		t.Fatalf("project should override shared: %+v", byName["shared"])
	}
	if byName["only-global"].Command != "tool-global" {
		t.Fatalf("only-global should survive from global: %+v", byName["only-global"])
	}
}

func TestMCPValidation(t *testing.T) {
	base := `
[[providers]]
name = "x"
endpoint = "https://x.example/v1"
model = "m"
`
	valid := func(extra string) string { return base + extra }
	cases := []struct {
		name    string
		section string
		want    string
	}{
		{"missing name", `
[[mcp_servers]]
transport = "sse"
url = "http://h:1/mcp/sse"`, "missing a name"},
		{"missing transport", `
[[mcp_servers]]
name = "x"
url = "http://h:1/mcp/sse"`, "missing a transport"},
		{"bad transport", `
[[mcp_servers]]
name = "x"
transport = "carrier-pigeon"
url = "http://h:1/mcp/sse"`, "transport must be one of stdio|sse|http"},
		{"stdio without command", `
[[mcp_servers]]
name = "x"
transport = "stdio"`, "missing a command"},
		{"sse without url", `
[[mcp_servers]]
name = "x"
transport = "sse"`, "endpoint"},
		{"http bad url", `
[[mcp_servers]]
name = "x"
transport = "http"
url = "not-a-url"`, "endpoint"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "c.toml")
			write(t, p, valid(c.section))
			_, err := Load(p)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
		})
	}

	// All three transports validate.
	dir := t.TempDir()
	p := filepath.Join(dir, "ok.toml")
	write(t, p, valid(`
[[mcp_servers]]
name = "sse"
transport = "sse"
url = "http://h:1/mcp/sse"
[[mcp_servers]]
name = "http"
transport = "http"
url = "https://h:2/mcp"
[[mcp_servers]]
name = "stdio"
transport = "stdio"
command = "tool"
args = ["-x"]
env = { A = "1" }
`))
	if _, err := Load(p); err != nil {
		t.Fatalf("valid mcp_servers rejected: %v", err)
	}

	// Empty list is fine (MCP is optional).
	p2 := filepath.Join(t.TempDir(), "ok2.toml")
	write(t, p2, valid(""))
	if _, err := Load(p2); err != nil {
		t.Fatalf("config without mcp_servers rejected: %v", err)
	}
}

func TestValidateMCPServersDuplicateNames(t *testing.T) {
	// Merge dedupes TOML files by name, but programmatic construction can
	// still produce duplicates — Validate must reject them.
	err := validateMCPServers([]MCPServer{
		{Name: "a", Transport: MCPTransportSSE, URL: "http://h:1/mcp/sse"},
		{Name: "a", Transport: MCPTransportStdio, Command: "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate mcp_server name") {
		t.Fatalf("err = %v, want duplicate mcp_server name", err)
	}
}

func TestContextWindowAndAutoCompressDecode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	write(t, p, `
[[providers]]
name = "local"
endpoint = "http://127.0.0.1:8080/v1"
model = "m"
context_window = 65536

[session]
auto_compress_threshold = 60
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	prov, ok := cfg.Provider("local")
	if !ok {
		t.Fatal("provider missing")
	}
	if prov.ContextWindow != 65536 {
		t.Fatalf("context_window = %d, want 65536", prov.ContextWindow)
	}
	if cfg.Session.AutoCompressThresholdPct() != 60 {
		t.Fatalf("threshold pct = %d, want 60", cfg.Session.AutoCompressThresholdPct())
	}
	if !cfg.Session.AutoCompressEnabled() {
		t.Fatal("expected auto-compress enabled at 60")
	}
}

func TestAutoCompressDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	write(t, p, `
[[providers]]
name = "local"
endpoint = "http://127.0.0.1:8080/v1"
model = "m"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Session.AutoCompressThresholdPct(); got != DefaultAutoCompressThreshold {
		t.Fatalf("threshold pct = %d, want default %d", got, DefaultAutoCompressThreshold)
	}
	if !cfg.Session.AutoCompressEnabled() {
		t.Fatal("auto-compress should default to enabled")
	}
	if cfg.Session.AutoCompressThreshold == nil {
		t.Fatal("applyDefaults should materialize the threshold pointer")
	}
}

func TestAutoCompressDisabledWithZero(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	write(t, p, `
[[providers]]
name = "local"
endpoint = "http://127.0.0.1:8080/v1"
model = "m"

[session]
auto_compress_threshold = 0
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Session.AutoCompressEnabled() {
		t.Fatal("auto-compress should be disabled when threshold is 0")
	}
}

func TestAutoCompressThresholdMergeProjectOverrides(t *testing.T) {
	dir := t.TempDir()
	global := filepath.Join(dir, "global.toml")
	project := filepath.Join(dir, "project.toml")

	write(t, global, `
[[providers]]
name = "a"
endpoint = "https://a.example/v1"
model = "ma"
[session]
auto_compress_threshold = 80
`)
	// A project setting 0 (disable) must override the global 80.
	write(t, project, `
[[providers]]
name = "a"
endpoint = "https://a.example/v1"
model = "ma"
[session]
auto_compress_threshold = 0
`)
	cfg, _, err := loadFrom(global, project)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Session.AutoCompressEnabled() {
		t.Fatal("project threshold 0 should disable auto-compress over the global 80")
	}
}

func TestValidateContextWindowNegative(t *testing.T) {
	cfg := Default()
	cfg.Providers = []Provider{{Name: "x", Endpoint: "https://x.example/v1", Model: "m", ContextWindow: -1}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "context_window") {
		t.Fatalf("err = %v, want context_window error", err)
	}
}

func TestValidateAutoCompressThresholdOutOfRange(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	write(t, p, `
[[providers]]
name = "x"
endpoint = "https://x.example/v1"
model = "m"
[session]
auto_compress_threshold = 150
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "auto_compress_threshold") {
		t.Fatalf("err = %v, want auto_compress_threshold error", err)
	}
}
