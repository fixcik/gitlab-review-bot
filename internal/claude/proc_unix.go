//go:build unix

package claude

import (
	"os/exec"
	"syscall"
)

// setProcessGroup makes cancellation kill the whole process tree of a local
// command (claude spawns tool subprocesses; sh -c spawns builds).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return cmd.Process.Kill()
	}
}
