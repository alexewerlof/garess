//go:build linux

package sandbox

import (
	"fmt"
	"log/slog"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Landlock UAPI constants not exposed by x/sys/unix (linux/landlock.h).
const (
	landlockCreateRulesetVersion = 1 << 0 // LANDLOCK_CREATE_RULESET_VERSION
	landlockRestrictSelfTSync    = 1 << 3 // LANDLOCK_RESTRICT_SELF_TSYNC
)

// rulesetAttr mirrors struct landlock_ruleset_attr. Only the first field is
// declared: the kernel zero-pads smaller user buffers, so an 8-byte attribute
// is accepted by every ABI and keeps older kernels (which know only this
// field) working.
type rulesetAttr struct {
	handledAccessFS uint64
}

// pathBeneathAttr mirrors struct landlock_path_beneath_attr.
type pathBeneathAttr struct {
	allowedAccess uint64
	parentFd      int32
}

// Available reports the kernel Landlock ABI (0 when unsupported) and whether
// process-wide enforcement (TSYNC, ABI >= 6) is possible. It never restricts
// anything.
func Available() (abi int, ok bool) {
	abi = detectABI()
	return abi, abi >= 6
}

// detectABI returns the kernel Landlock ABI (>= 1) or 0 when unsupported.
// It is a variable so tests can simulate kernels without Landlock (or with an
// ABI too old for process-wide TSYNC) without touching a real kernel.
var detectABI = func() int {
	// landlock_create_ruleset(NULL, 0, LANDLOCK_CREATE_RULESET_VERSION):
	// on success the syscall returns the ABI version as its value.
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, landlockCreateRulesetVersion)
	if errno != 0 {
		return 0
	}
	return int(fd)
}

// applyLandlock enforces the write-confinement domain on the calling process.
// It returns an Err Result for any failure to activate, so "auto" can degrade
// and "landlock" can abort startup.
func applyLandlock(opts Options) Result {
	abi := detectABI()
	if abi <= 0 {
		// No Landlock at all: like every failure to activate, this must be an
		// Err Result so "auto" degrades to none and the explicit "landlock"
		// backend aborts startup (fail closed) instead of silently running
		// unsandboxed.
		return Result{Backend: BackendNone,
			Err: fmt.Errorf("kernel has no Landlock support (needs Linux >= 5.13 with Landlock enabled)")}
	}
	// Process-wide enforcement relies on landlock_restrict_self(TSYNC),
	// introduced with Landlock ABI 6 (kernel 6.7). Earlier ABIs can only
	// lock the calling thread, which would leave the multi-threaded Go
	// runtime — and thus the tools — largely unrestricted.
	if abi < 6 {
		return Result{Backend: BackendNone, ABI: abi,
			Err: fmt.Errorf("Landlock ABI %d < 6: process-wide TSYNC enforcement needs kernel >= 6.7", abi)}
	}

	workDir := opts.WorkDir
	if workDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return Result{Backend: BackendNone, ABI: abi, Err: fmt.Errorf("get working directory: %w", err)}
		}
		workDir = wd
	}
	dirs := uniqueCleaned(append(DefaultWriteDirs(workDir), opts.WriteDirs...))
	if len(dirs) == 0 {
		return Result{Backend: BackendNone, ABI: abi, Err: fmt.Errorf("no writable directories configured")}
	}

	handled := writeRights & supportedFSMask(abi)
	rfd, err := createRuleset(handled)
	if err != nil {
		return Result{Backend: BackendNone, ABI: abi, Err: fmt.Errorf("create ruleset: %w", err)}
	}
	defer unix.Close(rfd)

	// A path_beneath rule on a directory covers everything beneath it now and
	// in the future (future files are created through the MAKE_* rights on the
	// parent), but the top-level directory itself must exist when the rule is
	// added. Ensure that, skipping and warning about anything unusable.
	var allowed []string
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			slog.Warn("sandbox: cannot create writable dir, skipping", "dir", dir, "err", err)
			continue
		}
		dfd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			slog.Warn("sandbox: cannot open writable dir, skipping", "dir", dir, "err", err)
			continue
		}
		err = addPathBeneath(rfd, dfd, handled)
		unix.Close(dfd)
		if err != nil {
			return Result{Backend: BackendNone, ABI: abi, Err: fmt.Errorf("allow writes under %s: %w", dir, err)}
		}
		allowed = append(allowed, dir)
	}
	if len(allowed) == 0 {
		return Result{Backend: BackendNone, ABI: abi, Err: fmt.Errorf("no writable directory could be allowed")}
	}

	// no_new_privs is a Landlock prerequisite (unprivileged sandboxing).
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return Result{Backend: BackendNone, ABI: abi, Err: fmt.Errorf("prctl(no_new_privs): %w", err)}
	}
	// TSYNC applies the domain to every thread of the process atomically.
	if err := restrictSelf(rfd, landlockRestrictSelfTSync); err != nil {
		return Result{Backend: BackendNone, ABI: abi, Err: fmt.Errorf("restrict self (TSYNC): %w", err)}
	}

	return Result{
		Backend: BackendLandlock,
		ABI:     abi,
		Active:  true,
		Reason:  fmt.Sprintf("write access confined to %d writable dir(s); reads and execution unrestricted", len(allowed)),
	}
}

func createRuleset(handled uint64) (int, error) {
	attr := rulesetAttr{handledAccessFS: handled}
	rfd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return -1, errno
	}
	return int(rfd), nil
}

func addPathBeneath(rulesetFD, dirFD int, allowed uint64) error {
	attr := pathBeneathAttr{allowedAccess: allowed, parentFd: int32(dirFD)}
	_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(rulesetFD), uintptr(unix.LANDLOCK_RULE_PATH_BENEATH),
		uintptr(unsafe.Pointer(&attr)), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func restrictSelf(rulesetFD int, flags uint64) error {
	_, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(rulesetFD), uintptr(flags), 0)
	if errno != 0 {
		return errno
	}
	return nil
}
