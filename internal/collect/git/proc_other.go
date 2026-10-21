//go:build !linux

package git

import "os/exec"

// Process-group isolation is implemented for Linux/WSL only; elsewhere the
// default cancellation (kill the direct child) applies and is unverified.
func isolateProcess(*exec.Cmd) {}

func killProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
