//go:build windows

package hooks

import (
	"os"
	"os/exec"
)

// setHookProcGroup is a no-op on Windows: there are no POSIX process groups.
// Hooks run `sh -c` and need /bin/sh at runtime anyway, so they are
// effectively unsupported on Windows — this keeps the package compiling for
// the windows release target.
func setHookProcGroup(*exec.Cmd) {}

// killHookProcGroup kills the hook's direct child process. There is no
// killpg(2) on Windows; os.Process.Kill terminates the single process, which
// is all that can be tracked here.
func killHookProcGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
