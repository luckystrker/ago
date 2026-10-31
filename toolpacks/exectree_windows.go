//go:build windows

package toolpacks

import (
	"fmt"
	"os/exec"
	"strconv"
)

// setupTreeKill arranges for a context cancel to kill the whole process tree,
// not just the direct child: `taskkill /T /F` walks the parent-child chain and
// force-terminates everything below the child. A cmd killed at the deadline
// would otherwise leave its grandchildren — programs started with `start`,
// background jobs — running on the host with inherited handles.
// AGO-DEVIATION: Python's LocalSandbox relies on subprocess.run(timeout=...),
// which kills only the direct child; AGo kills the tree (security audit
// 2026-10-31, L-13).
func setupTreeKill(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		kill := exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
		// With nil stdout/stderr the kill's own per-process log lines go to
		// the null device, not into the tool result.
		if err := kill.Run(); err != nil {
			return fmt.Errorf("taskkill /PID %d /T /F: %w", cmd.Process.Pid, err)
		}
		return nil
	}
}
