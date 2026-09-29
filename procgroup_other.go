//go:build !unix

package main

import (
	"os/exec"
	"time"
)

// inProcessGroup: without process groups, at least stop waiting on pipes held
// open by orphaned children.
func inProcessGroup(cmd *exec.Cmd) {
	cmd.WaitDelay = 3 * time.Second
}
