// Command garess is a minimal AI harness: an OpenAI-compatible chat client
// with a Claude-Code-style TUI, project/global memory and session transcripts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	_ "net/http/pprof" // GARESS_PPROF_ADDR exposes /debug/pprof for CPU profiling
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"garess/internal/agents"
	"garess/internal/chat"
	"garess/internal/config"
	"garess/internal/harness"
	"garess/internal/llm"
	"garess/internal/mcp"
	"garess/internal/memory"
	"garess/internal/sandbox"
	"garess/internal/skills"
	"garess/internal/tools"
	"garess/internal/tui"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "garess:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("garess", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", "", "path to a config file (default: global + project)")
	providerName := fs.String("provider", "", "provider to use (overrides default_provider)")
	modelName := fs.String("model", "", "model to use (overrides the provider default)")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		// -h / -help / --help are not defined flags: the flag package reports
		// flag.ErrHelp (its default usage is already discarded) — show our own
		// help instead of failing with an error.
		if errors.Is(err, flag.ErrHelp) {
			printUsage()
			return nil
		}
		return err
	}
	if *showVersion {
		fmt.Printf("garess %s\n", version)
		return nil
	}

	switch fs.Arg(0) {
	case "":
		setupLogging()
		return runTUI(*cfgPath, *providerName, *modelName)
	case "init":
		return runInit(fs.Args()[1:])
	case "doctor":
		return runDoctor(fs.Args()[1:])
	case "help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q (run 'garess help')", fs.Arg(0))
	}
}

func printUsage() {
	fmt.Print(`garess — a minimal AI harness

Usage:
  garess [flags]            start the chat TUI
  garess init [flags]       write a starter config to edit
  garess doctor [flags]     check config, endpoint and sandbox support
  garess help               show this help

Flags:
  -h, --help        show this help
  --config <path>   use an explicit config file
  --provider <name> provider to use (overrides default_provider)
  --model <name>    model to use (overrides the provider default)
  --version         print the version and exit

Init flags:
  -g    write to the global config location (~/.config/garess/config.toml)
  -f    overwrite an existing config file
`)
}

func printInitUsage() {
	fmt.Print(`Usage: garess init [-g] [-f]

Writes a starter config from the bundled example-config.toml, ready for you
to edit. By default writes ./config.toml in the current folder.

  -g    write to the global config location (~/.config/garess/config.toml)
  -f    overwrite an existing config file
`)
}

func printDoctorUsage() {
	fmt.Print(`Usage: garess doctor [--config <path>]

Checks the configuration, endpoint connectivity, hooks, MCP servers and
sandbox support.

  --config <path>   use an explicit config file
`)
}

// loadConfig loads the configuration and converts errors into friendly,
// actionable messages (not found vs. found-but-invalid).
func loadConfig(cfgPath string) (*config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, friendlyConfigError(err)
	}
	return cfg, nil
}

// friendlyConfigError turns typed config errors into messages that tell the
// user what happened and what to do about it.
func friendlyConfigError(err error) error {
	var nf *config.NotFoundError
	if errors.As(err, &nf) {
		var b strings.Builder
		b.WriteString("no config file found.\n\nLooked in:\n")
		for _, p := range nf.Searched {
			if abs, aerr := filepath.Abs(p); aerr == nil {
				p = abs
			}
			fmt.Fprintf(&b, "  - %s\n", p)
		}
		b.WriteString("\nCreate one with:\n  garess init      # writes ./config.toml in the current folder\n  garess init -g   # writes the global ~/.config/garess/config.toml\n")
		return errors.New(strings.TrimSuffix(b.String(), "\n"))
	}

	var inv *config.InvalidConfigError
	if errors.As(err, &inv) {
		if errors.Is(inv.Err, config.ErrNoProviders) {
			return fmt.Errorf("config %s was found but defines no LLM providers.\nAdd at least one [[providers]] block with name, endpoint and model (the starter config written by 'garess init' shows the format).", inv.Path)
		}
		return fmt.Errorf("config %s is invalid: %v", inv.Path, inv.Err)
	}
	return err
}

// setupLogging routes structured logs to a file, keeping the TUI on stdout.
func setupLogging() {
	path, err := config.GlobalLogPath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
}

func runTUI(cfgPath, providerName, modelName string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	for _, w := range cfg.Warnings {
		slog.Warn(w)
	}

	// Optional CPU profiling endpoint: GARESS_PPROF_ADDR=:6060. While the TUI
	// is slow, capture a profile from another SSH session with:
	//   curl -o /tmp/garess.pprof 'http://<pi>:6060/debug/pprof/profile?seconds=30'
	if addr := os.Getenv("GARESS_PPROF_ADDR"); addr != "" {
		go func() {
			if err := http.ListenAndServe(addr, nil); err != nil {
				slog.Warn("pprof server", "addr", addr, "err", err)
			}
		}()
		slog.Info("pprof server listening", "addr", addr)
	}

	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	globalDir, err := config.GlobalDir()
	if err != nil {
		return fmt.Errorf("resolve config dir: %w", err)
	}
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		return err
	}
	globalMem, err := config.GlobalMemoryDir()
	if err != nil {
		return err
	}
	mem := memory.New(config.ProjectMemoryDir(), globalMem)
	svc := chat.NewService(config.ProjectSessionsDir(), cfg.Session.HistoryLimit)
	sessID := chat.NewSessionID()
	preamble := harness.NewPreamble()

	// --model overrides the default provider's model before building.
	current := cfg.DefaultProvider
	if providerName != "" {
		if _, ok := cfg.Provider(providerName); !ok {
			return fmt.Errorf("provider %q not found in config", providerName)
		}
		current = providerName
	}
	if modelName != "" {
		p, ok := cfg.Provider(current)
		if !ok {
			return fmt.Errorf("provider %q not found in config", current)
		}
		p.Model = modelName
	}

	opts := harness.Options{
		Memory:         mem,
		WorkDir:        wd,
		SessionService: svc,
		Policy:         tools.DefaultPolicy(),
		Hooks:          cfg.Hooks,
		MCPServers:     cfg.MCPServers,
		Preamble:       preamble.Get,
	}
	providers := make(map[string]*harness.Provider, len(cfg.Providers))
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		prov, err := harness.Build(*p, opts)
		if err != nil {
			return fmt.Errorf("provider %q: %w", p.Name, err)
		}
		providers[p.Name] = prov
	}

	// Apply the Landlock tool sandbox (Phase 4). It is process-wide and
	// irreversible, so it runs last — after every directory garess needs has
	// been created — and before the first user turn. GARESS_SANDBOX overrides
	// the config backend for quick testing.
	backend := cfg.Sandbox.Backend
	if env := os.Getenv("GARESS_SANDBOX"); env != "" {
		backend = env
	}
	sres := sandbox.Apply(sandbox.Options{
		Backend:   backend,
		WorkDir:   wd,
		WriteDirs: cfg.Sandbox.WriteDirs,
	})
	if sres.Err != nil {
		return fmt.Errorf("sandbox: %w", sres.Err)
	}
	switch {
	case sres.Active:
		slog.Info(sres.String())
	case backend == sandbox.BackendLandlock || backend == sandbox.BackendAuto:
		slog.Warn(sres.String())
	}

	model, err := tui.New(providers, current, cfg.TUI.Theme, "local", sessID, mem, preamble, wd, 0, 0, tui.Options{
		AutoCompress:    cfg.Session.AutoCompressEnabled(),
		AutoCompressPct: cfg.Session.AutoCompressThresholdPct(),
		ContextWindows:  resolveContextWindows(cfg),
		Version:         version,
		SessionService:  svc, // right session rail + /sessions resume
	})
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(model, tea.WithAltScreen()).Run()
	return err
}

// contextProbeTimeout bounds each /v1/models context-window probe at startup.
const contextProbeTimeout = 1500 * time.Millisecond

// resolveContextWindows maps provider names to their context windows in
// tokens: the configured context_window wins; providers without one are
// probed via GET /v1/models (best effort, non-fatal, bounded) so the context
// indicator is right without manual config.
func resolveContextWindows(cfg *config.Config) map[string]int {
	out := make(map[string]int, len(cfg.Providers))
	var wg sync.WaitGroup
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.ContextWindow > 0 {
			out[p.Name] = p.ContextWindow
			continue
		}
		wg.Add(1)
		go func(p *config.Provider) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), contextProbeTimeout)
			defer cancel()
			if w, err := llm.LookupContextWindow(ctx, p.Endpoint, config.ResolveAPIKey(p), p.Model); err == nil && w > 0 {
				out[p.Name] = w
			}
		}(p)
	}
	wg.Wait()
	return out
}

// runInit writes a starter config — the bundled example-config.toml — ready
// for the user to edit. It writes ./config.toml in the current folder by
// default, or the global config location with -g (creating the directory and
// using 0600, since a global config may hold API keys). It refuses to
// overwrite an existing file unless -f is given.
func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	global := fs.Bool("g", false, "write the global config (~/.config/garess/config.toml)")
	force := fs.Bool("f", false, "overwrite an existing config file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printInitUsage()
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("init takes no arguments (got %q)", fs.Arg(0))
	}

	dest := config.RootConfigPath()
	mode := os.FileMode(0o644)
	if *global {
		var err error
		dest, err = config.GlobalConfigPath()
		if err != nil {
			return fmt.Errorf("resolve global config path: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		mode = 0o600
	}

	if _, err := os.Stat(dest); err == nil && !*force {
		return fmt.Errorf("%s already exists — edit it, or pass -f to overwrite it", dest)
	}
	if err := os.WriteFile(dest, config.Example(), mode); err != nil {
		return err
	}
	fmt.Printf("created %s — edit it, then run 'garess doctor' to verify\n", dest)
	return nil
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", "", "path to a config file (default: global + project)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printDoctorUsage()
			return nil
		}
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	p, err := cfg.DefaultProviderConfig()
	if err != nil {
		return err
	}
	key := config.ResolveAPIKey(p)
	fmt.Printf("provider:   %s\n", p.Name)
	fmt.Printf("endpoint:   %s\n", p.Endpoint)
	fmt.Printf("model:      %s\n", p.Model)
	fmt.Printf("api key:    %s\n", maskKey(key))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := llm.Ping(ctx, p.Endpoint, key); err != nil {
		fmt.Printf("connect:    FAIL — %v\n", err)
		return err
	}
	fmt.Println("connect:    OK")
	if models, err := llm.ListModels(ctx, p.Endpoint, key); err == nil {
		sorted := append([]string(nil), models...)
		sort.Strings(sorted)
		sample := sorted
		if len(sample) > 5 {
			sample = sample[:5]
		}
		fmt.Printf("models:     %d available (%s)\n", len(sorted), strings.Join(sample, ", "))
	}
	reportSandbox(cfg)
	reportHooks(cfg)
	reportMCP(cfg)
	reportContext(cfg)
	reportAgents()
	reportSkills()
	return nil
}

// kTokens renders a token count compactly ("128k").
func kTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
	return fmt.Sprintf("%d", n)
}

// reportContext reports the context-compression settings and each provider's
// resolved context window (configured, probed from /v1/models, or default).
func reportContext(cfg *config.Config) {
	fmt.Println("context:")
	if cfg.Session.AutoCompressEnabled() {
		fmt.Printf("  auto-compress: on (at %d%% of the context window)\n", cfg.Session.AutoCompressThresholdPct())
	} else {
		fmt.Println("  auto-compress: off (threshold 0)")
	}
	windows := resolveContextWindows(cfg)
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		w := windows[p.Name]
		src := "probed /v1/models"
		if p.ContextWindow > 0 {
			src = "configured"
		} else if w == 0 {
			w = config.DefaultContextWindow
			src = fmt.Sprintf("not advertised — default %s (configure context_window for accuracy)", kTokens(w))
		}
		fmt.Printf("  %-14s context window: %s tokens (%s)\n", p.Name+":", kTokens(w), src)
	}
}

func maskKey(k string) string {
	if k == "" {
		return "(none)"
	}
	if len(k) <= 8 {
		return "••••"
	}
	return k[:4] + "…" + k[len(k)-4:]
}

// reportSandbox reports kernel Landlock support and the configured sandbox
// (Phase 4).
func reportSandbox(cfg *config.Config) {
	abi, ok := sandbox.Available()
	fmt.Println("sandbox:")
	switch {
	case abi <= 0:
		fmt.Println("  landlock:    NOT supported by this kernel — tools run unsandboxed ('none' backend)")
	case !ok:
		fmt.Printf("  landlock:    ABI %d available but < 6 — process-wide TSYNC needs kernel >= 6.7, tools run unsandboxed\n", abi)
	default:
		fmt.Printf("  landlock:    ABI %d available (process-wide write confinement)\n", abi)
	}
	if k, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		fmt.Printf("  kernel:      %s\n", strings.TrimSpace(string(k)))
	}
	backend := cfg.Sandbox.Backend
	if env := os.Getenv("GARESS_SANDBOX"); env != "" {
		backend = env + " (GARESS_SANDBOX)"
	}
	fmt.Printf("  backend:     %s\n", backend)
	if len(cfg.Sandbox.WriteDirs) > 0 {
		fmt.Printf("  write dirs:  %s\n", strings.Join(cfg.Sandbox.WriteDirs, ", "))
	}
}

// reportMCP lists the configured MCP servers and, for each, connects and
// reports the tools the agent would get. A failing server is reported (with
// the reason) but does not fail doctor — at run time it degrades to no tools.
func reportMCP(cfg *config.Config) {
	if len(cfg.MCPServers) == 0 {
		fmt.Println("mcp_servers: none configured")
		return
	}
	fmt.Println("mcp_servers:")
	for _, s := range cfg.MCPServers {
		target := s.URL
		if s.Transport == config.MCPTransportStdio {
			target = s.Command
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		names, err := mcp.ListTools(ctx, s)
		cancel()
		if err != nil {
			fmt.Printf("  %-16s %s %s: FAIL — %v\n", s.Name, s.Transport, target, err)
			continue
		}
		if len(names) == 0 {
			fmt.Printf("  %-16s %s %s: no tools\n", s.Name, s.Transport, target)
			continue
		}
		fmt.Printf("  %-16s %s %s: %s\n", s.Name, s.Transport, target, strings.Join(names, ", "))
	}
}

// reportHooks lists the configured git-style hooks (Phase 3).
func reportHooks(cfg *config.Config) {
	if len(cfg.Hooks) == 0 {
		fmt.Println("hooks:      none configured")
		return
	}
	fmt.Println("hooks:")
	for _, h := range cfg.Hooks {
		timeout := h.Timeout
		if timeout == "" {
			timeout = "10s (default)"
		}
		fmt.Printf("  %-16s %s  [timeout %s]\n", h.Event, h.Command, timeout)
	}
}

// reportAgents lists the instruction files (AGENTS.md / SYSTEM.md) that apply
// to the working directory.
func reportAgents() {
	wd, err := os.Getwd()
	if err != nil {
		return
	}
	sources, err := agents.Discover(wd)
	if err != nil {
		fmt.Printf("agents:      error: %v\n", err)
		return
	}
	if len(sources) == 0 {
		fmt.Println("agents:      no AGENTS.md / SYSTEM.md files apply")
		return
	}
	fmt.Println("agents:")
	for _, s := range sources {
		fmt.Printf("  - %s (%s)\n", s.Path, s.Scope)
	}
}

// reportSkills lists the installed skill files that apply to the working directory.
func reportSkills() {
	wd, err := os.Getwd()
	if err != nil {
		return
	}
	sources, err := skills.Discover(wd)
	if err != nil {
		fmt.Printf("skills:      error: %v\n", err)
		return
	}
	if len(sources) == 0 {
		fmt.Println("skills:      no skill files apply")
		return
	}
	fmt.Println("skills:")
	for _, s := range sources {
		fmt.Printf("  - %s (%s · %s)\n", s.Path, s.Name, s.Scope)
	}
}
