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
//
// Setsid rather than Setpgid, which gives the same group (a session leader is a
// group leader with pgid == pid, so the negative-pid kill below is unchanged)
// and one thing more: the child has no CONTROLLING TERMINAL. That is what makes
// "never wait for a human" true for the processes lca starts rather than only
// for lca itself — stdin can be closed, but a git credential helper or a
// pinentry opens /dev/tty directly and asks there, which in an unattended run is
// a stall nobody can see and whose prompt text appears in neither captured
// stream. The two cannot be set together: setpgid(2) refuses a session leader
// with EPERM, so asking for both would fail the exec.
func inProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 3 * time.Second
}
