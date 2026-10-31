//go:build !windows

package toolpacks

import (
	"errors"
	"os/exec"
	"syscall"
)

// setupTreeKill arranges for a context cancel to kill the whole process tree,
// not just the direct child. The child is placed in its own process group
// (pgid = its pid), and the group is signaled with SIGKILL: a shell killed at
// the deadline would otherwise leave its grandchildren — background jobs,
// daemons started with `... &` — running on the host with inherited handles.
// AGO-DEVIATION: Python's LocalSandbox relies on subprocess.run(timeout=...),
// which kills only the direct child; AGo kills the group (security audit
// 2026-10-31, L-13).
func setupTreeKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// A negative pid addresses the group led by the child; ESRCH means the
		// group is already gone.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
}
