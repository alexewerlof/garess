// Package personas discovers and parses custom-agent definitions: Markdown
// files with YAML frontmatter describing specialized personas (name,
// description, tool allowlist, model, instructions). A persona can be run as a
// sub-agent by the main garess agent through the run_subagent tool (see
// internal/harness). The file format follows the VS Code custom-agent /
// Claude Code sub-agent convention, with garess-native discovery roots:
//
//   - user:    $XDG_CONFIG_HOME/garess/agents/*.md
//   - project: <launch dir>/.garess/agents/*.md and <launch dir>/agents/*.md
//
// Project definitions override user definitions of the same name (mirroring
// the config merge-by-name rule). The search is capped at the launch
// directory, like AGENTS.md/SYSTEM.md and skills discovery.
package personas

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"garess/internal/agents"
	"garess/internal/config"
)

// Scope values mirror internal/agents and internal/skills.
const (
	ScopeUser    = "user"
	ScopeProject = "project"
)

// DefaultUserInvocable is the frontmatter default for user-invocable.
const DefaultUserInvocable = true

// Persona is one parsed custom-agent definition.
type Persona struct {
	// Name is the agent identifier (frontmatter name, else the file stem).
	Name string
	// Description is shown to the model in the run_subagent tool and in
	// `garess doctor`.
	Description string
	// Tools is the allowlist of tool names this persona may use. Empty means
	// "all built-ins" (run_subagent is never implied and must be listed).
	Tools []string
	// Model overrides the model id on the active provider. Empty means the
	// main model.
	Model string
	// UserInvocable reports whether the persona may be started by the user
	// directly (parsed for forward compatibility; this milestone only runs
	// personas as sub-agents).
	UserInvocable bool
	// DisableModelInvocation prevents the persona from being invoked as a
	// sub-agent by other agents.
	DisableModelInvocation bool
	// Agents lists the sub-agent names this persona may delegate to via
	// run_subagent. Nil means any model-invocable persona; an empty list
	// means none. Only consulted when run_subagent is in Tools.
	Agents []string
	// ArgumentHint guides how the run_subagent task should be phrased.
	ArgumentHint string
	// Path is the absolute path of the definition file.
	Path string
	// Scope is "user" or "project".
	Scope string
	// Body is the persona's instruction body after @import and {{VAR}}
	// expansion.
	Body string
	// Warnings collects non-fatal parse/validation notes.
	Warnings []string
}

// ModelInvocable reports whether other agents may run this persona as a
// sub-agent (i.e. disable-model-invocation is not set).
func (p Persona) ModelInvocable() bool {
	return !p.DisableModelInvocation
}

// AllowSet returns the persona's allowed sub-agent names as a set for
// run_subagent registration: nil means any model-invocable persona, an empty
// map means none.
func (p Persona) AllowSet() map[string]bool {
	if p.Agents == nil {
		return nil
	}
	set := make(map[string]bool, len(p.Agents))
	for _, n := range p.Agents {
		set[n] = true
	}
	return set
}

// UserSearchPaths returns the user-level custom-agent directory.
func UserSearchPaths() string {
	dir, err := config.GlobalDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "agents")
}

// ProjectSearchPaths returns the custom-agent directories under dir,
// project-scoped and capped at dir.
func ProjectSearchPaths(dir string) ([]string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return []string{
		filepath.Join(abs, ".garess", "agents"),
		filepath.Join(abs, "agents"),
	}, nil
}

// Discover returns the custom-agent personas that apply to dir: user-global
// definitions first, then project definitions under dir (project overrides
// user by name). Files without a YAML frontmatter block and files that fail
// to parse are skipped with a warning logged, never fatal. The search is
// capped at dir.
func Discover(dir string) ([]Persona, error) {
	var out []Persona
	idx := map[string]int{}
	add := func(p Persona) {
		if i, ok := idx[p.Name]; ok {
			out[i] = p
			return
		}
		idx[p.Name] = len(out)
		out = append(out, p)
	}

	if u := UserSearchPaths(); u != "" {
		if err := collect(u, ScopeUser, add); err != nil {
			return nil, err
		}
	}
	projectRoots, err := ProjectSearchPaths(dir)
	if err != nil {
		return nil, err
	}
	for _, root := range projectRoots {
		if err := collect(root, ScopeProject, add); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func collect(root, scope string, add func(Persona)) error {
	if !dirExists(root) {
		return nil
	}
	paths, err := walkMarkdown(root)
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, p := range paths {
		persona, err := Parse(p, scope)
		if err != nil {
			slog.Warn("personas: skipping file", "path", p, "err", err)
			continue
		}
		add(persona)
	}
	return nil
}

func walkMarkdown(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subtrees
		}
		if d.IsDir() {
			// Skip hidden directories.
			if strings.HasPrefix(d.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(d.Name()), ".md") {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

// Parse reads one custom-agent file and returns its Persona. A missing or
// malformed YAML frontmatter block is an error (callers skip such files).
func Parse(path, scope string) (Persona, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Persona{}, err
	}
	fmText, body, ok := agents.SplitFrontmatter(string(b))
	if !ok {
		return Persona{}, fmt.Errorf("no YAML frontmatter (--- ... ---) block")
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
		return Persona{}, fmt.Errorf("frontmatter: %v", err)
	}
	name := strings.TrimSpace(fm.Name)
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	userInvocable := DefaultUserInvocable
	if fm.UserInvocable != nil {
		userInvocable = *fm.UserInvocable
	}
	rendered, err := agents.RenderText(body, filepath.Dir(path))
	if err != nil {
		return Persona{}, fmt.Errorf("render body: %v", err)
	}
	return Persona{
		Name:                   name,
		Description:            strings.TrimSpace(fm.Description),
		Tools:                  fm.Tools,
		Model:                  strings.TrimSpace(fm.Model),
		UserInvocable:          userInvocable,
		DisableModelInvocation: fm.DisableModelInvocation,
		Agents:                 fm.Agents,
		ArgumentHint:           strings.TrimSpace(fm.ArgumentHint),
		Path:                   path,
		Scope:                  scope,
		Body:                   strings.TrimSpace(rendered),
	}, nil
}

// Validate drops tool names that are not in knownTools (the built-ins plus
// run_subagent) into Warnings and returns a copy. An empty Tools list keeps
// its "all built-ins" meaning and is unchanged.
func (p Persona) Validate(knownTools []string) Persona {
	known := make(map[string]bool, len(knownTools))
	for _, k := range knownTools {
		known[k] = true
	}
	var kept []string
	for _, t := range p.Tools {
		if known[t] {
			kept = append(kept, t)
		} else {
			p.Warnings = append(p.Warnings, fmt.Sprintf("unknown tool %q ignored", t))
		}
	}
	p.Tools = kept
	return p
}

// frontmatter mirrors the supported YAML fields. tools accepts a list or a
// comma-separated string (VS Code and Claude formats).
type frontmatter struct {
	Name                   string     `yaml:"name"`
	Description            string     `yaml:"description"`
	Tools                  toolsField `yaml:"tools"`
	Model                  string     `yaml:"model"`
	UserInvocable          *bool      `yaml:"user-invocable"`
	DisableModelInvocation bool       `yaml:"disable-model-invocation"`
	Agents                 []string   `yaml:"agents"`
	ArgumentHint           string     `yaml:"argument-hint"`
}

type toolsField []string

// UnmarshalYAML accepts either a YAML list of names or a scalar
// comma-separated string ("Read, Grep, Bash"), the Claude format.
func (t *toolsField) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		var out []string
		for _, part := range strings.Split(s, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		*t = out
		return nil
	}
	var list []string
	if err := value.Decode(&list); err != nil {
		return err
	}
	*t = list
	return nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
