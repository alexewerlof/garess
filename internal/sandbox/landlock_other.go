//go:build !linux

package sandbox

// Available reports that Landlock is unavailable off Linux.
func Available() (abi int, ok bool) { return 0, false }

// applyLandlock is a no-op on non-Linux systems; the only backend is "none".
func applyLandlock(opts Options) Result {
	return Result{Backend: BackendNone, Reason: "Landlock is Linux-only; sandboxing disabled"}
}
