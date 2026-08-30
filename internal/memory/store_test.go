package memory

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"foo", "foo.md", false},
		{"foo.md", "foo.md", false},
		{"notes.2026-08", "notes.2026-08", false}, // "2026-08" is seen as an extension
		{"a_b-c", "a_b-c.md", false},
		{"", "", true},
		{".hidden", "", true},
		{"../etc/passwd", "", true},
		{"a/b", "", true},
		{"a b", "", true},
		{"a$b", "", true},
	}
	for _, c := range cases {
		got, err := SanitizeName(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("SanitizeName(%q): expected error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("SanitizeName(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("SanitizeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWriteReadDelete(t *testing.T) {
	base := t.TempDir()
	st := New(filepath.Join(base, "local"), filepath.Join(base, "global"))

	if err := st.Write("alpha", "hello local", false); err != nil {
		t.Fatal(err)
	}
	if err := st.Write("alpha", "hello global", true); err != nil {
		t.Fatal(err)
	}

	got, local, err := st.Read("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !local || got != "hello local" {
		t.Fatalf("local should shadow global: local=%v content=%q", local, got)
	}

	if err := st.Delete("alpha", true); err != nil {
		t.Fatal(err)
	}
	got, local, err = st.Read("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !local || got != "hello local" {
		t.Fatalf("global delete broke local: local=%v content=%q", local, got)
	}

	if err := st.Delete("alpha", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Read("alpha"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected ErrNotExist, got %v", err)
	}
}

func TestDeleteMissing(t *testing.T) {
	st := New(filepath.Join(t.TempDir(), "local"), filepath.Join(t.TempDir(), "global"))
	if err := st.Delete("nope", false); err == nil {
		t.Fatal("expected an error deleting a missing note")
	}
}

func TestList(t *testing.T) {
	base := t.TempDir()
	st := New(filepath.Join(base, "local"), filepath.Join(base, "global"))
	if err := st.Write("b.md", "b", false); err != nil {
		t.Fatal(err)
	}
	if err := st.Write("a.md", "a-local", false); err != nil {
		t.Fatal(err)
	}
	if err := st.Write("a.md", "a-global", true); err != nil {
		t.Fatal(err)
	}
	if err := st.Write("c.md", "c", true); err != nil {
		t.Fatal(err)
	}

	notes, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 3 {
		t.Fatalf("got %d notes, want 3", len(notes))
	}
	if notes[0].Name != "a.md" || notes[0].Scope != "local" {
		t.Fatalf("a.md should be local (shadowing global): %+v", notes[0])
	}
	if notes[2].Name != "c.md" || notes[2].Scope != "global" {
		t.Fatalf("c.md should be global: %+v", notes[2])
	}
}

func TestWriteTraversalRejected(t *testing.T) {
	st := New(filepath.Join(t.TempDir(), "local"), filepath.Join(t.TempDir(), "global"))
	if err := st.Write("../evil", "x", false); err == nil {
		t.Fatal("expected traversal name to be rejected")
	}
}
