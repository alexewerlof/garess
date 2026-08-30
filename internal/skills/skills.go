package skills

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"garess/internal/agents"
)

// Source describes one discovered skill file.
type Source struct {
	Name  string // skill directory name
	Path  string // absolute path to the skill file
	Scope string // "user" or "project"
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

// Discover returns each skill file in scope for dir.
func Discover(dir string) ([]Source, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var sources []Source
	for _, root := range UserSearchPaths() {
		found, err := collect(root, "user", seen)
		if err != nil {
			return nil, err
		}
		sources = append(sources, found...)
	}
	for _, root := range []string{
		filepath.Join(abs, ".garess", "skills"),
		filepath.Join(abs, "skills"),
	} {
		found, err := collect(root, "project", seen)
		if err != nil {
			return nil, err
		}
		sources = append(sources, found...)
	}
	return sources, nil
}

// Build assembles the text for all skills in scope.
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
		parts = append(parts, fmt.Sprintf("## Skill (%s · %s · %s)\n\n%s", s.Name, s.Scope, s.Path, strings.TrimSpace(body)))
	}
	return strings.Join(parts, "\n\n---\n\n"), nil
}

// Render reads a single skill file, expanding imports and variables.
func Render(path string) (string, error) {
	return agents.Render(path)
}

func collect(root, scope string, seen map[string]bool) ([]Source, error) {
	if root == "" {
		return nil, nil
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Source
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
		out = append(out, Source{
			Name:  filepath.Base(filepath.Dir(absPath)),
			Path:  absPath,
			Scope: scope,
		})
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
