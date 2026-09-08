package skills

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"garess/internal/agents"
)

// Skill is one discovered and parsed skill file. A skill's YAML frontmatter
// (if any) is parsed for its name and description only; unknown keys are
// ignored and the frontmatter block is stripped, so arbitrary YAML in a skill
// file is never passed to the model. The directory name is the skill name
// (the frontmatter name field is not used, mirroring the discovery
// convention).
type Skill struct {
	Name        string // skill directory name
	Description string // frontmatter description ("" when absent)
	Path        string // absolute path to the skill file
	Scope       string // "user" or "project"
	Body        string // frontmatter-stripped body, imports and vars expanded
}

// UserSearchPaths returns the user-level skill roots to inspect, in order.
func UserSearchPaths() []string {
	var paths []string
	if base, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(base, "garess", "skills"))
		paths = append(paths, filepath.Join(base, "skills"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".garess", "skills"))
		paths = append(paths, filepath.Join(home, ".skills"))
	}
	return dedupe(paths)
}

// Discover returns each parsed skill file in scope for dir. Files with a
// malformed YAML frontmatter block are skipped with a warning (never fatal),
// so one broken skill cannot hide the rest.
func Discover(dir string) ([]Skill, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Skill
	for _, root := range UserSearchPaths() {
		found, err := collect(root, "user", seen)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	for _, root := range []string{
		filepath.Join(abs, ".garess", "skills"),
		filepath.Join(abs, "skills"),
	} {
		found, err := collect(root, "project", seen)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

// Build assembles the text for all skills in scope: each skill is rendered as
// "## Skill (name · scope · path)" followed by its frontmatter description (if
// any) and its stripped, rendered body.
func Build(dir string) (string, error) {
	skills, err := Discover(dir)
	if err != nil {
		return "", err
	}
	if len(skills) == 0 {
		return "", nil
	}
	var parts []string
	for _, s := range skills {
		var sb strings.Builder
		fmt.Fprintf(&sb, "## Skill (%s · %s · %s)", s.Name, s.Scope, s.Path)
		if s.Description != "" {
			sb.WriteString("\n\n")
			sb.WriteString(s.Description)
		}
		if s.Body != "" {
			sb.WriteString("\n\n")
			sb.WriteString(s.Body)
		}
		parts = append(parts, sb.String())
	}
	return strings.Join(parts, "\n\n---\n\n"), nil
}

// Parse reads and parses one skill file: optional YAML frontmatter is parsed
// for name and description, the frontmatter block is stripped from the body,
// and the body is rendered (@import lines and {{VAR}} placeholders expanded).
// A malformed frontmatter block is an error.
func Parse(path, scope string) (Skill, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Skill{}, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return Skill{}, err
	}
	fmText, rawBody, hasFM := agents.SplitFrontmatter(string(b))
	desc := ""
	if hasFM {
		var fm struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
		}
		if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
			return Skill{}, fmt.Errorf("frontmatter: %v", err)
		}
		desc = strings.TrimSpace(fm.Description)
		// Unknown frontmatter keys are ignored here: yaml.Unmarshal into this
		// struct filters them out, so they never reach the model.
	} else {
		rawBody = string(b)
	}
	rendered, err := agents.RenderText(rawBody, filepath.Dir(abs))
	if err != nil {
		return Skill{}, err
	}
	return Skill{
		Name:        filepath.Base(filepath.Dir(abs)),
		Description: desc,
		Path:        abs,
		Scope:       scope,
		Body:        strings.TrimSpace(rendered),
	}, nil
}

// Render returns a skill file's body with any frontmatter stripped and
// imports/variables expanded. Parse returns the full parsed skill.
func Render(path string) (string, error) {
	sk, err := Parse(path, "")
	if err != nil {
		return "", err
	}
	return sk.Body, nil
}

func collect(root, scope string, seen map[string]bool) ([]Skill, error) {
	if root == "" {
		return nil, nil
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Skill
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := strings.ToLower(d.Name())
		if name != "skill.md" && name != "readme.md" {
			return nil
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if seen[absPath] {
			return nil
		}
		seen[absPath] = true
		sk, err := Parse(absPath, scope)
		if err != nil {
			slog.Warn("skills: skipping file", "path", absPath, "err", err)
			return nil
		}
		out = append(out, sk)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func dedupe(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
