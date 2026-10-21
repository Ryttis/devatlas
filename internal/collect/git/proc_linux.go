//go:build linux

package git

import (
	"os/exec"
	"syscall"
)

// isolateProcess runs git in its own process group and kills the whole
// group on cancellation, so helper processes started by git cannot keep the
// output pipe open past the deadline.
func isolateProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func killProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
