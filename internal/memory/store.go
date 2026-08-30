// Package memory implements local (project) and global (~/.config) memory
// notes as plain markdown files.
package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Note describes a stored memory note.
type Note struct {
	Name    string
	Path    string
	Scope   string // "local" or "global"
	ModTime time.Time
	Size    int64
}

// Store reads and writes notes in two scopes: a project-local directory and a
// global directory.
type Store struct {
	LocalDir  string
	GlobalDir string
}

// New creates a store for the given directories.
func New(localDir, globalDir string) *Store {
	return &Store{LocalDir: localDir, GlobalDir: globalDir}
}

// SanitizeName validates a note name and appends ".md" when it has no
// extension. Returns an error for names that could escape the store.
func SanitizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("empty note name")
	}
	if strings.HasPrefix(name, ".") {
		return "", errors.New("note name must not start with '.'")
	}
	if strings.ContainsAny(name, `/\`) {
		return "", errors.New("note name must not contain path separators")
	}
	if !nameRe.MatchString(name) {
		return "", errors.New("note name may only contain letters, digits, '.', '_' and '-'")
	}
	if filepath.Ext(name) == "" {
		name += ".md"
	}
	return name, nil
}

// List returns all notes (local shadowing global), sorted by name.
func (s *Store) List() ([]Note, error) {
	var out []Note
	seen := map[string]bool{}
	scopes := []struct{ dir, name string }{{s.LocalDir, "local"}, {s.GlobalDir, "global"}}
	for _, sc := range scopes {
		if sc.dir == "" {
			continue
		}
		entries, err := os.ReadDir(sc.dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || seen[e.Name()] {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			seen[e.Name()] = true
			out = append(out, Note{
				Name:    e.Name(),
				Path:    filepath.Join(sc.dir, e.Name()),
				Scope:   sc.name,
				ModTime: info.ModTime(),
				Size:    info.Size(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Read returns a note's content. Local notes shadow global ones. The returned
// bool reports whether the note came from the local scope.
func (s *Store) Read(name string) (string, bool, error) {
	n, err := SanitizeName(name)
	if err != nil {
		return "", false, err
	}
	for _, d := range []string{s.LocalDir, s.GlobalDir} {
		if d == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d, n))
		if err == nil {
			return string(b), d == s.LocalDir, nil
		}
		if !os.IsNotExist(err) {
			return "", false, err
		}
	}
	return "", false, os.ErrNotExist
}

// Write stores a note in the given scope (global=true → global).
func (s *Store) Write(name, content string, global bool) error {
	n, err := SanitizeName(name)
	if err != nil {
		return err
	}
	dir := s.LocalDir
	if global {
		dir = s.GlobalDir
	}
	if dir == "" {
		return errors.New("store has no directory for this scope")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, n), []byte(content), 0o644)
}

// Delete removes a note from the given scope.
func (s *Store) Delete(name string, global bool) error {
	n, err := SanitizeName(name)
	if err != nil {
		return err
	}
	dir, scope := s.LocalDir, "local"
	if global {
		dir, scope = s.GlobalDir, "global"
	}
	if dir == "" {
		return errors.New("store has no directory for this scope")
	}
	p := filepath.Join(dir, n)
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("note %q not found in %s scope", n, scope)
		}
		return err
	}
	return nil
}
