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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"garess/internal/agents"
	"garess/internal/chat"
	"garess/internal/config"
	"garess/internal/harness"
	"garess/internal/llm"
	"garess/internal/memory"
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
	case "doctor":
		return runDoctor(fs.Args()[1:])
	case "help", "-h", "--help":
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
  garess doctor [flags]     check config, endpoint and sandbox support
  garess help               show this help

Flags:
  --config <path>   use an explicit config file
  --provider <name> provider to use (overrides default_provider)
  --model <name>    model to use (overrides the provider default)
  --version         print the version and exit
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
		if len(nf.Searched) > 0 {
			p := nf.Searched[0]
			fmt.Fprintf(&b, "\nCreate one by copying example-config.toml:\n  mkdir -p %s\n  cp example-config.toml %s\n", filepath.Dir(p), p)
		}
		return errors.New(strings.TrimSuffix(b.String(), "\n"))
	}

	var inv *config.InvalidConfigError
	if errors.As(err, &inv) {
		if errors.Is(inv.Err, config.ErrNoProviders) {
			return fmt.Errorf("config %s was found but defines no LLM providers.\nAdd at least one [[providers]] block with name, endpoint and model — see example-config.toml.", inv.Path)
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

	model, err := tui.New(providers, current, cfg.TUI.Theme, "local", sessID, mem, preamble, wd, 0, 0)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(model, tea.WithAltScreen()).Run()
	return err
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", "", "path to a config file (default: global + project)")
	if err := fs.Parse(args); err != nil {
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
	reportSandbox()
	reportAgents()
	reportSkills()
	return nil
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

// reportSandbox checks kernel support for the Landlock sandbox (Phase 4).
func reportSandbox() {
	fmt.Println("sandbox:")
	lsm, err := os.ReadFile("/sys/kernel/security/lsm")
	switch {
	case err != nil:
		fmt.Println("  landlock:    unknown (cannot read /sys/kernel/security/lsm)")
	case strings.Contains(string(lsm), "landlock"):
		fmt.Println("  landlock:    available (kernel-enforced tool sandboxing)")
	default:
		fmt.Println("  landlock:    NOT enabled in this kernel — tool sandboxing falls back to 'none'")
	}
	if k, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		fmt.Printf("  kernel:      %s\n", strings.TrimSpace(string(k)))
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
