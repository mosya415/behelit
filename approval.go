package main

import (
	"bufio"
	"fmt"
	"strings"
)

// The soft gate. read_file/grep run automatically (no side effects); edit,
// write and run_command are shown to the user and require an explicit "y".
// This is deliberately advisory — the jail (jail.go) is the hard boundary.
// Both are required: the gate catches intent, the jail catches reach.

func needsApproval(name string) bool {
	switch name {
	case "edit", "write", "run_command":
		return true
	}
	return false
}

// Approver owns the interactive gate and the session-wide "approve all" mode.
// It is the single place a side-effecting action is confirmed, so every path is
// recorded consistently — including whether the yes was typed by the user or
// granted automatically by the trust mode (audited via the `auto` return).
type Approver struct {
	in      *bufio.Reader
	autoAll bool // when set, every action is approved without prompting
}

func NewApprover(in *bufio.Reader) *Approver {
	return &Approver{in: in}
}

// Confirm reports whether an action may proceed and whether it was auto-granted.
// Prompt answers:
//
//	y / yes  approve just this action
//	n / <Enter>  deny (default)
//	a / all  approve this and auto-approve every later action this session
func (a *Approver) Confirm(header, preview string) (approved, auto bool) {
	if a.autoAll {
		fmt.Printf("\033[90m● auto-approved (session): %s\033[0m\n", header)
		return true, true
	}

	fmt.Printf("\n\033[33m● approval required: %s\033[0m\n", header)
	if preview != "" {
		fmt.Println(preview)
	}
	fmt.Print("apply? [y/N/a=all] ")

	line, err := a.in.ReadString('\n')
	if err != nil {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, false
	case "a", "all":
		a.autoAll = true
		fmt.Println("\033[90m  (auto-approve enabled for this session — /approve off to disable)\033[0m")
		return true, false
	default:
		return false, false
	}
}

func (a *Approver) SetAuto(on bool) { a.autoAll = on }
func (a *Approver) Auto() bool      { return a.autoAll }

func (a *Approver) Mode() string {
	if a.autoAll {
		return "auto-approve (session)"
	}
	return "prompt each action"
}
