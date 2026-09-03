//go:build !windows

package hooks

import (
	"os/exec"
	"syscall"
)

// setHookProcGroup runs the hook in its own process group (Setpgid). This is
// what lets a timeout kill the hook's whole process group (-pid) instead of
// just sh: many shells fork the command (dash runs `sh -c "sleep 5"` as sh
// plus a child sleep), and killing only the direct child leaves orphaned
// grandchildren holding the captured-output pipe open, which makes
// cmd.Wait() block until they exit on their own — defeating the timeout.
func setHookProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killHookProcGroup kills the hook's whole process group (-pid). It is
// invoked by cmd.Cancel when the hook's context expires (and by cmd.Wait if
// WaitDelay elapses).
func killHookProcGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
