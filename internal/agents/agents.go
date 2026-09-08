// Package agents discovers and renders instruction files: AGENTS.md (the
// agents.md convention, https://agents.md) plus a lightweight pi.dev-style
// SYSTEM.md for the same purpose.
//
// AGENTS.md files give the model project/user instructions: user-level files
// from $XDG_CONFIG_HOME/agents/AGENTS.md (or ~/.agents/AGENTS.md) apply
// everywhere, and the AGENTS.md in the directory where the harness was
// launched applies to that project. SYSTEM.md mirrors pi.dev: a single
// user-level file in garess's own config directory
// ($XDG_CONFIG_HOME/garess/SYSTEM.md) and a single project file in the launch
// directory. The assembled text is injected into the model context as a
// system message.
package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Source describes one discovered instruction file (AGENTS.md or SYSTEM.md).
type Source struct {
	Path  string // absolute path to the instruction file
	Scope string // "user" or "project"
}

// UserSearchPaths returns the user-level AGENTS.md locations, in order:
// $XDG_CONFIG_HOME/agents/AGENTS.md (or ~/.config/agents/AGENTS.md), then
// ~/.agents/AGENTS.md.
func UserSearchPaths() []string {
	var paths []string
	if base, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(base, "agents", "AGENTS.md"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".agents", "AGENTS.md"))
	}
	return paths
}

// SystemSearchPaths returns the user-level SYSTEM.md location: garess's own
// config directory, mirroring pi.dev's ~/.pi/agent/SYSTEM.md. On Linux this
// resolves to $XDG_CONFIG_HOME/garess/SYSTEM.md or ~/.config/garess/SYSTEM.md.
func SystemSearchPaths() []string {
	var paths []string
	if base, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(base, "garess", "SYSTEM.md"))
	}
	return paths
}

// Discover returns the instruction files that apply to dir: user-global
// AGENTS.md and SYSTEM.md files, then the AGENTS.md and SYSTEM.md in the
// directory itself. The search is capped at dir — parent directories are not
// walked, so only the files where the harness was launched are used for the
// project scope.
func Discover(dir string) ([]Source, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	var sources []Source
	for _, p := range UserSearchPaths() {
		if fileExists(p) {
			sources = append(sources, Source{Path: p, Scope: "user"})
		}
	}
	for _, p := range SystemSearchPaths() {
		if fileExists(p) {
			sources = append(sources, Source{Path: p, Scope: "user"})
		}
	}
	if p := filepath.Join(abs, "AGENTS.md"); fileExists(p) {
		sources = append(sources, Source{Path: p, Scope: "project"})
	}
	if p := filepath.Join(abs, "SYSTEM.md"); fileExists(p) {
		sources = append(sources, Source{Path: p, Scope: "project"})
	}
	return sources, nil
}

// Build assembles the instruction text that applies to dir: each discovered
// AGENTS.md or SYSTEM.md is rendered (imports and variables expanded) and
// wrapped with a header naming the file. It returns "" when no instruction
// files apply.
func Build(dir string) (string, error) {
	sources, err := Discover(dir)
	if err != nil {
		return "", err
	}
	if len(sources) == 0 {
		return "", nil
	}
	var parts []string
	for _, s := range sources {
		body, err := Render(s.Path)
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("## %s (%s · %s)\n\n%s", filepath.Base(s.Path), s.Scope, s.Path, strings.TrimSpace(body)))
	}
	return strings.Join(parts, "\n\n---\n\n"), nil
}

// Render reads a single AGENTS.md file, expanding @import lines (relative to
// the file's directory, globs supported) and {{VAR}} placeholders from the
// environment.
func Render(path string) (string, error) {
	return render(path, nil, 0)
}

// RenderText expands @import lines and {{VAR}} placeholders in arbitrary
// markdown text, resolving imports relative to baseDir. Instruction files and
// custom-agent (persona) bodies share the same rendering engine.
func RenderText(text, baseDir string) (string, error) {
	return expandText(text, baseDir, nil, 0)
}

func render(path string, seen map[string]bool, depth int) (string, error) {
	if depth > 8 {
		return "", fmt.Errorf("too many nested imports at %s", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if seen == nil {
		seen = map[string]bool{}
	}
	if seen[abs] {
		return "", fmt.Errorf("import cycle detected at %s", abs)
	}
	seen[abs] = true

	b, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	return expandText(string(b), filepath.Dir(abs), seen, depth)
}

// expandText processes one text block (a file body or a persona body),
// expanding @import whole lines relative to baseDir (globs supported) and
// {{VAR}} environment placeholders. Imports recurse through render, so cycle
// detection and the depth cap apply across file boundaries; depth is the
// current file-import depth of the block.
func expandText(text, baseDir string, seen map[string]bool, depth int) (string, error) {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if target, ok := importTarget(line); ok {
			matches, err := filepath.Glob(filepath.Join(baseDir, target))
			if err != nil {
				return "", fmt.Errorf("%s: bad import %q: %v", baseDir, target, err)
			}
			if len(matches) == 0 {
				return "", fmt.Errorf("%s: import %q matches no files", baseDir, target)
			}
			sort.Strings(matches)
			for _, m := range matches {
				sub, err := render(m, seen, depth+1)
				if err != nil {
					return "", err
				}
				out = append(out, sub)
			}
			continue
		}
		out = append(out, expandVars(line))
	}
	return strings.Join(out, "\n"), nil
}

// importTarget extracts an @import target from a line, e.g. "@docs/style.md".
func importTarget(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "@") {
		return "", false
	}
	target := strings.TrimSpace(strings.TrimPrefix(trimmed, "@"))
	if target == "" || strings.ContainsAny(target, " \t") {
		return "", false
	}
	return target, true
}

// expandVars replaces {{NAME}} placeholders with the value of the NAME
// environment variable (unknown variables expand to the empty string).
func expandVars(line string) string {
	var sb strings.Builder
	for {
		start := strings.Index(line, "{{")
		if start < 0 {
			sb.WriteString(line)
			return sb.String()
		}
		end := strings.Index(line[start:], "}}")
		if end < 0 {
			sb.WriteString(line)
			return sb.String()
		}
		end += start + len("}}")
		sb.WriteString(line[:start])
		sb.WriteString(os.Getenv(line[start+2 : end-2]))
		line = line[end:]
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
