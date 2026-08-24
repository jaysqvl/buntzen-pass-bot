//go:build darwin || linux

package actionproc

import (
	"os/exec"
	"syscall"
)

// Each action owns a process group so a forced shutdown cannot leave Chromium
// descendants running with a locked persistent profile.
func configureProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}
