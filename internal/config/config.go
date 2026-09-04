package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	// DefaultHistoryLimit is the number of messages kept in the model context.
	DefaultHistoryLimit = 40
	// DefaultTheme is used when the theme is unset or "auto".
	DefaultTheme = "dark"
)

// ErrNotFound is returned when a config file does not exist.
var ErrNotFound = errors.New("config file not found")

// ErrNoProviders indicates a config file was found but defines no usable
// LLM providers.
var ErrNoProviders = errors.New("no LLM providers configured: add at least one [[providers]] block with name, endpoint and model")

// NotFoundError is returned when no config file could be found. It lists the
// paths that were searched.
type NotFoundError struct {
	Searched []string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("no config file found (looked in: %s)", strings.Join(e.Searched, ", "))
}

// InvalidConfigError is returned when a config file was found but could not
// be used (parse error, missing providers, invalid values).
type InvalidConfigError struct {
	Path string
	Err  error
}

func (e *InvalidConfigError) Error() string {
	return fmt.Sprintf("config %s: %v", e.Path, e.Err)
}

func (e *InvalidConfigError) Unwrap() error { return e.Err }

// Config is the top-level garess configuration.
type Config struct {
	// DefaultProvider names the provider used when none is selected.
	DefaultProvider string `toml:"default_provider"`
	// Providers is the list of named OpenAI-compatible endpoints.
	Providers []Provider `toml:"providers"`
	// TUI holds terminal UI options.
	TUI TUI `toml:"tui"`
	// Session holds conversation options.
	Session Session `toml:"session"`
	// Hooks are git-style shell hooks run on agent/tool/model/session events.
	Hooks []Hook `toml:"hooks"`
	// Sandbox holds the Landlock tool-sandbox options (Phase 4).
	Sandbox Sandbox `toml:"sandbox"`
	// MCPServers are MCP (Model Context Protocol) servers whose tools are
	// exposed to the agent alongside the built-ins.
	MCPServers []MCPServer `toml:"mcp_servers"`

	// Warnings are non-fatal notes collected while decoding (e.g. unknown keys).
	Warnings []string `toml:"-"`
}

// Sandbox holds the tool-sandbox options. The sandbox kernel-enforces a
// write allowlist over the whole garess process (tool file operations AND
// bash children): reads and execution stay unrestricted everywhere; writes,
// creates, removes and truncates are only permitted under the writable
// directories. See the internal/sandbox package for the backend semantics.
type Sandbox struct {
	// Backend selects the enforcement backend:
	//   - "none" (default): no sandboxing.
	//   - "auto": use Landlock when the kernel supports it (ABI >= 6 for
	//     process-wide TSYNC), otherwise fall back to "none".
	//   - "landlock": require Landlock; startup fails if it cannot apply.
	Backend string `toml:"backend"`
	// WriteDirs are extra absolute directories the sandbox may write to,
	// on top of the garess defaults (workdir, project state, /tmp and the
	// global config dir). Only meaningful with a Landlock backend — useful
	// for build caches (GOCACHE, ~/.cache, ...). Project config replaces the
	// global list.
	WriteDirs []string `toml:"write_dirs"`
}

// SandboxBackends lists the accepted sandbox.backend values.
var SandboxBackends = []string{"none", "auto", "landlock"}

// Hook fires a shell command when a named agent event occurs. See the
// internal/hooks package for the event names, the invocation contract
// (sh -c, JSON payload on stdin, argv[1] = event) and which events abort on
// a non-zero exit.
type Hook struct {
	// Event is one of the internal/hooks events, e.g. "before_tool",
	// "after_model", "on_event", "before_run".
	Event string `toml:"event"`
	// Command is the shell command to run (sh -c). It receives the event name
	// as $1 and a JSON event payload on stdin.
	Command string `toml:"command"`
	// Timeout bounds the hook run (Go duration, e.g. "5s"). Empty means the
	// hooks package default (10s).
	Timeout string `toml:"timeout"`
}

// MCP transport names. stdio spawns a local subprocess; sse and http connect
// to a remote endpoint (Server-Sent Events and streamable HTTP respectively).
const (
	// MCPTransportStdio spawns a local command and speaks MCP over its
	// stdin/stdout.
	MCPTransportStdio = "stdio"
	// MCPTransportSSE connects to a Server-Sent Events endpoint.
	MCPTransportSSE = "sse"
	// MCPTransportHTTP connects to a streamable HTTP endpoint.
	MCPTransportHTTP = "http"
)

// MCPTransports lists the accepted mcp_servers.transport values.
var MCPTransports = []string{MCPTransportStdio, MCPTransportSSE, MCPTransportHTTP}

// MCPServer describes one MCP server to expose to the agent. See the
// internal/mcp package for the client wiring and the graceful-degradation
// semantics (an unreachable server skips its tools for the turn instead of
// failing the run).
type MCPServer struct {
	// Name is a unique identifier used in logs, doctor output and the
	// GARESS_MCP_<NAME>_TOKEN environment lookup (name uppercased).
	Name string `toml:"name"`
	// Transport selects the connection: stdio | sse | http.
	Transport string `toml:"transport"`
	// URL is the endpoint for sse and http transports.
	URL string `toml:"url"`
	// Command is the executable to spawn for the stdio transport.
	Command string `toml:"command"`
	// Args are the arguments passed to Command (stdio only).
	Args []string `toml:"args"`
	// Env are extra environment variables for the stdio subprocess.
	Env map[string]string `toml:"env"`
	// Headers are extra HTTP headers sent on every request to sse/http
	// servers. The GARESS_MCP_<NAME>_TOKEN environment variable, when set,
	// overrides an Authorization header configured here.
	Headers map[string]string `toml:"headers"`
}

// Provider describes a single OpenAI-compatible endpoint.
type Provider struct {
	Name     string `toml:"name"`
	Endpoint string `toml:"endpoint"`
	APIKey   string `toml:"api_key"`
	Model    string `toml:"model"`
}

// TUI holds terminal UI options.
type TUI struct {
	Theme string `toml:"theme"` // auto | dark | light
}

// Session holds conversation options.
type Session struct {
	HistoryLimit int `toml:"history_limit"`
}

// Default returns a configuration with sane defaults and no providers.
func Default() *Config {
	return &Config{
		TUI:     TUI{Theme: DefaultTheme},
		Session: Session{HistoryLimit: DefaultHistoryLimit},
		Sandbox: Sandbox{Backend: "none"},
	}
}

// Load reads the configuration.
//
// If explicitPath is non-empty only that file is used (and must exist).
// Otherwise SearchPaths are consulted: the global config is loaded first and
// overlaid with the first project-local config that exists. Defaults are
// applied and the result is validated.
//
// Errors are typed so callers can tell the cases apart:
//   - *NotFoundError when no config file could be found (Searched lists where)
//   - *InvalidConfigError when a found file is unparseable or invalid (Err
//     wraps the specific problem, e.g. ErrNoProviders)
func Load(explicitPath string) (*Config, error) {
	if explicitPath != "" {
		if err := requireFile(explicitPath); err != nil {
			return nil, &NotFoundError{Searched: []string{explicitPath}}
		}
		cfg, _, err := loadFrom("", explicitPath)
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}
	return loadSearch(SearchPaths())
}

// SearchPaths returns the config files Load looks for, in order: global
// (~/.config/garess/config.toml), then project-local (.garess/config.toml),
// then project-root (config.toml). The first project file that exists is used.
func SearchPaths() []string {
	var paths []string
	if g, err := GlobalConfigPath(); err == nil {
		paths = append(paths, g)
	}
	paths = append(paths, ProjectConfigPath(), RootConfigPath())
	return paths
}

// loadSearch merges the global config with the first existing project config
// from searched[1:], applying defaults and validation.
func loadSearch(searched []string) (*Config, error) {
	global := ""
	if len(searched) > 0 {
		global = searched[0]
	}
	project := ""
	for _, p := range searched[1:] {
		if fileExists(p) {
			project = p
			break
		}
	}
	cfg, found, err := loadFrom(global, project)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &NotFoundError{Searched: searched}
	}
	return cfg, nil
}

// loadFrom merges an optional global and project config, applies defaults and
// validates the result. The bool reports whether at least one file was found.
func loadFrom(globalPath, projectPath string) (*Config, bool, error) {
	cfg := Default()
	var warnings []string
	found := false
	for _, p := range []string{globalPath, projectPath} {
		if p == "" {
			continue
		}
		over, w, err := loadFile(p)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, false, &InvalidConfigError{Path: p, Err: err}
		}
		found = true
		cfg.Merge(over)
		warnings = append(warnings, w...)
	}
	if !found {
		return cfg, false, nil
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		path := projectPath
		if path == "" {
			path = globalPath
		}
		return nil, false, &InvalidConfigError{Path: path, Err: err}
	}
	cfg.Warnings = warnings
	return cfg, true, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func requireFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory, not a config file", path)
	}
	return nil
}

// loadFile decodes a single TOML file into a fresh Config.
func loadFile(path string) (*Config, []string, error) {
	cfg := &Config{}
	md, err := toml.DecodeFile(path, cfg)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var warnings []string
	for _, k := range md.Undecoded() {
		warnings = append(warnings, fmt.Sprintf("%s: unknown key %q ignored", path, k.String()))
	}
	return cfg, warnings, nil
}

// Merge overlays a secondary config onto c. Providers are merged by name
// (project overrides global); scalar fields override when non-zero.
func (c *Config) Merge(over *Config) {
	if over == nil {
		return
	}
	if over.DefaultProvider != "" {
		c.DefaultProvider = over.DefaultProvider
	}
	for _, p := range over.Providers {
		replaced := false
		for i := range c.Providers {
			if c.Providers[i].Name == p.Name {
				c.Providers[i] = p
				replaced = true
				break
			}
		}
		if !replaced {
			c.Providers = append(c.Providers, p)
		}
	}
	if over.TUI.Theme != "" {
		c.TUI.Theme = over.TUI.Theme
	}
	if over.Session.HistoryLimit != 0 {
		c.Session.HistoryLimit = over.Session.HistoryLimit
	}
	if over.Sandbox.Backend != "" {
		c.Sandbox.Backend = over.Sandbox.Backend
	}
	if len(over.Sandbox.WriteDirs) > 0 {
		c.Sandbox.WriteDirs = over.Sandbox.WriteDirs
	}
	c.Hooks = mergeHooks(c.Hooks, over.Hooks)
	for _, s := range over.MCPServers {
		replaced := false
		for i := range c.MCPServers {
			if c.MCPServers[i].Name == s.Name {
				c.MCPServers[i] = s
				replaced = true
				break
			}
		}
		if !replaced {
			c.MCPServers = append(c.MCPServers, s)
		}
	}
}

// mergeHooks overlays project hooks onto global hooks: when the project
// defines any hook for an event it replaces the global hooks for that event
// entirely (so a project can drop a global guard); events the project does
// not touch keep their global hooks. Order within an event is preserved.
func mergeHooks(base, over []Hook) []Hook {
	if len(over) == 0 {
		return base
	}
	overEvents := make(map[string]bool, len(over))
	for _, h := range over {
		overEvents[h.Event] = true
	}
	out := make([]Hook, 0, len(base)+len(over))
	for _, h := range base {
		if !overEvents[h.Event] {
			out = append(out, h)
		}
	}
	return append(out, over...)
}

func (c *Config) applyDefaults() {
	if c.TUI.Theme == "" {
		c.TUI.Theme = DefaultTheme
	}
	if c.Session.HistoryLimit == 0 {
		c.Session.HistoryLimit = DefaultHistoryLimit
	}
	if c.Sandbox.Backend == "" {
		c.Sandbox.Backend = "none"
	}
	if c.DefaultProvider == "" && len(c.Providers) > 0 {
		c.DefaultProvider = c.Providers[0].Name
	}
}

// Validate checks the configuration for consistency.
func (c *Config) Validate() error {
	if len(c.Providers) == 0 {
		return ErrNoProviders
	}
	names := make(map[string]bool, len(c.Providers))
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Name == "" {
			return fmt.Errorf("provider #%d is missing a name", i+1)
		}
		if names[p.Name] {
			return fmt.Errorf("duplicate provider name %q", p.Name)
		}
		names[p.Name] = true
		if err := validateEndpoint(p.Endpoint); err != nil {
			return fmt.Errorf("provider %q: %w", p.Name, err)
		}
		if p.Model == "" {
			return fmt.Errorf("provider %q is missing a model", p.Name)
		}
	}
	if !names[c.DefaultProvider] {
		return fmt.Errorf("default_provider %q does not match any configured provider", c.DefaultProvider)
	}
	switch c.TUI.Theme {
	case "auto", "dark", "light":
	default:
		return fmt.Errorf("tui.theme must be one of auto|dark|light, got %q", c.TUI.Theme)
	}
	switch c.Sandbox.Backend {
	case "none", "auto", "landlock":
	default:
		return fmt.Errorf("sandbox.backend must be one of none|auto|landlock, got %q", c.Sandbox.Backend)
	}
	for _, d := range c.Sandbox.WriteDirs {
		if d == "" {
			return fmt.Errorf("sandbox.write_dirs entries must not be empty")
		}
		if !filepath.IsAbs(d) {
			return fmt.Errorf("sandbox.write_dirs entry %q must be an absolute path", d)
		}
	}
	if err := validateMCPServers(c.MCPServers); err != nil {
		return err
	}
	return nil
}

// validateMCPServers checks the mcp_servers list for consistency: unique
// names and transport-appropriate targets.
func validateMCPServers(servers []MCPServer) error {
	names := make(map[string]bool, len(servers))
	for i := range servers {
		s := &servers[i]
		if s.Name == "" {
			return fmt.Errorf("mcp_server #%d is missing a name", i+1)
		}
		if names[s.Name] {
			return fmt.Errorf("duplicate mcp_server name %q", s.Name)
		}
		names[s.Name] = true
		switch s.Transport {
		case MCPTransportStdio:
			if s.Command == "" {
				return fmt.Errorf("mcp_server %q (stdio) is missing a command", s.Name)
			}
		case MCPTransportSSE, MCPTransportHTTP:
			if err := validateEndpoint(s.URL); err != nil {
				return fmt.Errorf("mcp_server %q: %w", s.Name, err)
			}
		case "":
			return fmt.Errorf("mcp_server %q is missing a transport (one of %s)", s.Name, strings.Join(MCPTransports, "|"))
		default:
			return fmt.Errorf("mcp_server %q transport must be one of %s, got %q", s.Name, strings.Join(MCPTransports, "|"), s.Transport)
		}
	}
	return nil
}

func validateEndpoint(endpoint string) error {
	if endpoint == "" {
		return errors.New("missing endpoint")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid endpoint %q: %v", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("endpoint %q must be an http(s) URL", endpoint)
	}
	if u.Host == "" {
		return fmt.Errorf("endpoint %q is missing a host", endpoint)
	}
	return nil
}

// Provider returns the named provider.
func (c *Config) Provider(name string) (*Provider, bool) {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i], true
		}
	}
	return nil, false
}

// DefaultProviderConfig returns the configured default provider.
func (c *Config) DefaultProviderConfig() (*Provider, error) {
	p, ok := c.Provider(c.DefaultProvider)
	if !ok {
		return nil, fmt.Errorf("default provider %q not configured", c.DefaultProvider)
	}
	return p, nil
}

// ResolveAPIKey returns the API key to use for a provider: the
// GA_RESS_API_KEY environment variable wins over the configured key.
func ResolveAPIKey(p *Provider) string {
	if k := os.Getenv("GA_RESS_API_KEY"); k != "" {
		return k
	}
	return p.APIKey
}
