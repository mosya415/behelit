package main

import (
	"bufio"
	"fmt"
	"sort"
	"strings"
)

// The soft gate. read_file/grep/list_dir run automatically (no side effects);
// edit, write and run_command are shown to the user and require an explicit "y".
// This is deliberately advisory — the jail (jail.go) is the hard boundary.
// Both are required: the gate catches intent, the jail catches reach.

func needsApproval(name string) bool {
	switch name {
	case "edit", "write", "run_command":
		return true
	}
	return false
}

// Side-effecting tools group into two trust CLASSES so the user can grant
// standing approval to one kind of action without the other — e.g. auto-run
// allowlisted commands while still confirming every file change.
//
//	"edit" — file mutations: edit, write
//	"run"  — command execution: run_command
func classOf(kind string) string {
	switch kind {
	case "edit", "write":
		return "edit"
	case "run_command", "run":
		return "run"
	}
	return kind
}

var allClasses = []string{"edit", "run"}

// Approver owns the interactive gate and the session's per-class trust set.
// It is the single place a side-effecting action is confirmed, so every path is
// recorded consistently — including whether the yes was typed by the user or
// granted automatically by a trusted class (audited via the `auto` return).
type Approver struct {
	in      *bufio.Reader
	trusted map[string]bool // trusted classes: "edit", "run"
}

func NewApprover(in *bufio.Reader) *Approver {
	return &Approver{in: in, trusted: map[string]bool{}}
}

func (a *Approver) Trust(class string) { a.trusted[classOf(class)] = true }
func (a *Approver) TrustAll() {
	for _, c := range allClasses {
		a.trusted[c] = true
	}
}
func (a *Approver) Clear()                   { a.trusted = map[string]bool{} }
func (a *Approver) Trusts(class string) bool { return a.trusted[classOf(class)] }

// TrustedClasses returns the trusted classes in stable order (for audit/display).
func (a *Approver) TrustedClasses() []string {
	var cs []string
	for c := range a.trusted {
		if a.trusted[c] {
			cs = append(cs, c)
		}
	}
	sort.Strings(cs)
	return cs
}

// Confirm reports whether an action of the given kind may proceed and whether it
// was auto-granted by a trusted class. Prompt answers:
//
//	y / yes  approve just this action
//	n / <Enter>  deny (default)
//	a / all  approve this and auto-approve every later action this session
func (a *Approver) Confirm(kind, header, preview string) (approved, auto bool) {
	class := classOf(kind)
	if a.trusted[class] {
		fmt.Printf(" %s%s AUTO-APPROVED%s %s\n", cFaint, gNone, cReset, header)
		return true, true
	}

	fmt.Printf("\n %s%s APPROVAL REQUIRED%s %s\n", cYellow, gUp, cReset, header)
	if preview != "" {
		fmt.Println(preview)
	}
	fmt.Print(" APPLY? " + cFaint + "[Y/N/A=ALL]" + cReset + " ")

	line, err := a.in.ReadString('\n')
	if err != nil {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, false
	case "a", "all":
		a.TrustAll()
		fmt.Println(" " + faint("%s AUTO-APPROVE ENABLED (session) — /approve off to disable", gNone))
		return true, false
	default:
		return false, false
	}
}

// Mode renders the current trust posture for the banner / status.
func (a *Approver) Mode() string {
	cs := a.TrustedClasses()
	switch {
	case len(cs) == 0:
		return "prompt each action"
	case len(cs) == len(allClasses):
		return "auto-approve all (session)"
	default:
		return "auto-approve: " + strings.Join(cs, ", ")
	}
}

// ModeShort is a compact form of Mode for the status line: "ask", "auto", or
// "auto:run,edit".
func (a *Approver) ModeShort() string {
	cs := a.TrustedClasses()
	switch {
	case len(cs) == 0:
		return "ask"
	case len(cs) == len(allClasses):
		return "auto"
	default:
		return "auto:" + strings.Join(cs, ",")
	}
}
