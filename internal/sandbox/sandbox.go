// Package sandbox implements the Phase 4 tool sandbox: an opt-in, kernel-
// enforced write allowlist for the whole garess process (backend "landlock"),
// with a "none" no-op backend and "auto" selection.
//
// Why write-confinement: garess is a coding agent — its tools must read
// files anywhere (configs, system files) and execute programs, but a
// misbehaving model should not be able to write, create or delete files
// outside the project. The Landlock backend therefore handles only the
// write-side access rights (WRITE_FILE, TRUNCATE, REMOVE_*, MAKE_*) and
// grants them under the writable directories. Reads and execution are not
// handled, so they stay permitted everywhere. Because the kernel enforces
// per-process, one domain confines both the in-process file tools
// (read_file/write_file/glob/grep/memory_*) and every bash child.
//
// Enforcement is process-wide and irreversible: Apply must be called exactly
// once, after garess has created every directory it needs and before any user
// turn. All-thread enforcement uses landlock_restrict_self() with
// LANDLOCK_RESTRICT_SELF_TSYNC, which needs Landlock ABI >= 6 (kernel >= 6.7).
// Older kernels without Landlock at all — including every Raspberry Pi OS
// armv6 `rpi-v6` kernel (verified ENOSYS on 6.18.34+rpt-rpi-v6) — fall back
// to the "none" backend.
package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	"garess/internal/config"
)

// Backend names (mirror config.SandboxBackends).
const (
	BackendNone     = "none"
	BackendAuto     = "auto"
	BackendLandlock = "landlock"
)

// Landlock filesystem access rights (linux/landlock.h). Read/execute rights
// are deliberately absent: they are never handled, so reads and execution
// stay allowed everywhere. MAKE_CHAR/MAKE_BLOCK are omitted too — a coding
// agent has no business creating device nodes.
const (
	accessFSWriteFile  uint64 = 1 << 1
	accessFSRemoveDir         = 1 << 4
	accessFSRemoveFile        = 1 << 5
	accessFSMakeDir           = 1 << 7
	accessFSMakeReg           = 1 << 8
	accessFSMakeSock          = 1 << 9
	accessFSMakeFIFO          = 1 << 10
	accessFSMakeSym           = 1 << 12
	accessFSTruncate          = 1 << 14
)

// writeRights is the handled-and-granted set: every mutating FS operation.
var writeRights uint64 = accessFSWriteFile | accessFSRemoveDir | accessFSRemoveFile |
	accessFSMakeDir | accessFSMakeReg | accessFSMakeSock | accessFSMakeFIFO |
	accessFSMakeSym | accessFSTruncate

// supportedFSMask returns the access-right bits the kernel knows for a given
// ABI: ABI1 has bits 0-12, ABI2 adds refer(13), ABI3 truncate(14), ABI5
// ioctl(15) and ABI9 resolve_unix(16). Passing unknown bits to the kernel
// fails with EINVAL, so handled rights must be masked to this.
func supportedFSMask(abi int) uint64 {
	bits := 13
	switch {
	case abi >= 9:
		bits = 17
	case abi >= 5:
		bits = 16
	case abi >= 3:
		bits = 15
	case abi >= 2:
		bits = 14
	}
	return (uint64(1) << bits) - 1
}

// Options configures Apply. Backend defaults to "none".
type Options struct {
	// Backend is none | auto | landlock.
	Backend string
	// WorkDir is the project root; it drives the default writable dirs.
	WorkDir string
	// WriteDirs are extra absolute writable dirs beyond the defaults.
	WriteDirs []string
}

// Result describes the outcome of Apply.
type Result struct {
	// Backend is the effective backend in place ("none" when inactive).
	Backend string
	// ABI is the detected Landlock ABI (0 when unavailable).
	ABI int
	// Active reports whether a Landlock write-confinement domain is enforced.
	Active bool
	// Err is a hard failure — the sandbox could not be enforced. Only the
	// explicit "landlock" backend propagates it; "auto" degrades to none.
	Err error
	// Reason is a human-readable note for logs and doctor.
	Reason string
}

// String renders a Result for logs and `garess doctor`.
func (r Result) String() string {
	if r.Err != nil {
		return fmt.Sprintf("sandbox backend %s (Landlock ABI %d): %v", r.Backend, r.ABI, r.Err)
	}
	return fmt.Sprintf("sandbox backend %s (Landlock ABI %d): %s", r.Backend, r.ABI, r.Reason)
}

// Apply enforces the configured sandbox on the calling process. It is
// process-wide and irreversible — call it exactly once at startup, after
// every directory garess needs has been created and before any user turn.
//
// "none" never fails. "auto" never fails either: if Landlock cannot be
// applied it degrades to "none" with an explanatory Result. "landlock"
// returns a Result with Err set when the domain cannot be applied, so
// callers can refuse to run unsandboxed.
func Apply(opts Options) Result {
	switch opts.Backend {
	case "", BackendNone:
		return Result{Backend: BackendNone, Reason: "sandboxing disabled (backend none)"}
	case BackendAuto:
		res := applyLandlock(opts)
		if res.Err != nil {
			return Result{Backend: BackendNone, ABI: res.ABI, Reason: "auto: " + res.Err.Error()}
		}
		return res
	case BackendLandlock:
		return applyLandlock(opts)
	default:
		return Result{Backend: BackendNone, Err: fmt.Errorf("unknown sandbox backend %q", opts.Backend)}
	}
}

// DefaultWriteDirs returns the writable directories garess always needs when
// a Landlock domain is active: the project workdir (which also covers the
// project-local .garess/ state), the system temp dir, and the global config
// dir (~/.config/garess, where global memory notes and logs live). Extra
// directories — typically build caches such as GOCACHE or ~/.cache — go in
// config [sandbox] write_dirs.
func DefaultWriteDirs(workDir string) []string {
	dirs := []string{workDir}
	if t := os.TempDir(); t != "" {
		dirs = append(dirs, t)
	}
	if g, err := config.GlobalDir(); err == nil && g != "" {
		dirs = append(dirs, g)
	}
	return uniqueCleaned(dirs)
}

// uniqueCleaned absolutizes, cleans and dedupes a directory list.
func uniqueCleaned(dirs []string) []string {
	seen := make(map[string]bool, len(dirs))
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if !filepath.IsAbs(d) {
			if abs, err := filepath.Abs(d); err == nil {
				d = abs
			}
		}
		d = filepath.Clean(d)
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}
