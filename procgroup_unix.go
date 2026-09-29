//go:build unix

package main

import (
	"os/exec"
	"syscall"
	"time"
)

// inProcessGroup makes cmd the leader of its own process group and, on
// cancellation or timeout, kills the whole group — a check that spawns a
// server or grandchildren (make, go test, npm) must not outlive its timeout
// or hold the output pipe open.
func inProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 3 * time.Second
}
