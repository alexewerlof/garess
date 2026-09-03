package sandbox

import (
	"path/filepath"
	"strings"
	"testing"
)

// Note: these unit tests never invoke Apply with a Landlock backend — that
// would sandbox the test process irreversibly. The real Landlock path is
// exercised in landlock_test.go via a helper subprocess.

func TestBackendSelection(t *testing.T) {
	// Default and "none" are inert no-ops.
	for _, backend := range []string{"", BackendNone} {
		res := Apply(Options{Backend: backend})
		if res.Backend != BackendNone || res.Active || res.Err != nil {
			t.Fatalf("backend %q: got %+v, want inactive none", backend, res)
		}
	}
	// An unknown backend is a hard error.
	res := Apply(Options{Backend: "firejail"})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "unknown sandbox backend") {
		t.Fatalf("unknown backend: got %+v, want error", res)
	}
}

func TestSupportedFSMask(t *testing.T) {
	// ABI1 knows bits 0-12 (no refer/truncate).
	if got := supportedFSMask(1); got != (1<<13)-1 {
		t.Fatalf("mask(1) = %#x", got)
	}
	// ABI2 adds refer (bit 13).
	if got := supportedFSMask(2); got != (1<<14)-1 {
		t.Fatalf("mask(2) = %#x", got)
	}
	// ABI3 adds truncate (bit 14).
	if got := supportedFSMask(3); got != (1<<15)-1 {
		t.Fatalf("mask(3) = %#x", got)
	}
	// Old ABIs must never include truncate (bit 14).
	if supportedFSMask(1)&accessFSTruncate != 0 || supportedFSMask(2)&accessFSTruncate != 0 {
		t.Fatal("truncate must not be handled below ABI 3")
	}
	// Modern kernels include it.
	if supportedFSMask(9)&accessFSTruncate == 0 {
		t.Fatal("truncate should be available at ABI 9")
	}
}

func TestWriteRightsAreWriteOnly(t *testing.T) {
	// The handled set must cover every mutating op and no read/exec bits.
	for _, abi := range []int{1, 3, 9} {
		handled := writeRights & supportedFSMask(abi)
		if handled&accessFSTruncate != 0 && abi < 3 {
			t.Fatalf("abi %d handles truncate unexpectedly", abi)
		}
		// execute(0), read_file(2), read_dir(3) must never be handled.
		for _, bit := range []uint64{1 << 0, 1 << 2, 1 << 3} {
			if handled&bit != 0 {
				t.Fatalf("abi %d handles read/exec bit %d: %#x", abi, bit, handled)
			}
		}
		// Every write-side op is present.
		want := accessFSWriteFile | accessFSRemoveDir | accessFSRemoveFile |
			accessFSMakeDir | accessFSMakeReg | accessFSMakeSock | accessFSMakeFIFO |
			accessFSMakeSym
		if abi >= 3 {
			want |= accessFSTruncate
		}
		if handled != want {
			t.Fatalf("abi %d handled = %#x, want %#x", abi, handled, want)
		}
	}
}

func TestUniqueCleaned(t *testing.T) {
	got := uniqueCleaned([]string{"", "/tmp/x/../x", "/tmp/x", "/abs"})
	want := []string{"/tmp/x", "/abs"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("uniqueCleaned = %v, want %v", got, want)
	}
	// Relative paths are absolutized.
	rel := uniqueCleaned([]string{"rel"})
	if !filepath.IsAbs(rel[0]) {
		t.Fatalf("relative dir not absolutized: %v", rel)
	}
}

func TestDefaultWriteDirsIncludeWorkDir(t *testing.T) {
	dirs := DefaultWriteDirs("/tmp/some/project")
	abs, _ := filepath.Abs("/tmp/some/project")
	found := false
	for _, d := range dirs {
		if d == filepath.Clean(abs) {
			found = true
		}
	}
	if !found {
		t.Fatalf("workdir missing from defaults: %v", dirs)
	}
}
