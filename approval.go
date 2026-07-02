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

func askApproval(in *bufio.Reader, header, preview string) bool {
	fmt.Printf("\n\033[33m● approval required: %s\033[0m\n", header)
	if preview != "" {
		fmt.Println(preview)
	}
	fmt.Print("apply? [y/N] ")
	line, err := in.ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}
