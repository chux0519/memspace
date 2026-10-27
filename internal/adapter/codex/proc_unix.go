//go:build !windows

package codex

import (
	"os/exec"
	"syscall"
)

// `codex` is a Node wrapper around a native binary. Run it in its own process
// group so a timeout kills both, instead of orphaning the native process.
func setProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
