//go:build !unix

package main

import "errors"

// makeRaw is unsupported off unix (Windows); the caller falls back to a cooked
// line read — history and the command menu need raw mode.
func makeRaw(fd int) (func(), error) {
	return nil, errors.New("raw mode unsupported on this platform")
}

func makeTurnMode(fd int) (func(), error) {
	return nil, errors.New("terminal modes unsupported on this platform")
}

// isTerminal has no termios to ask here. False is the safe answer: every caller
// reads it as "nobody to ask" and refuses instead of reading.
func isTerminal(fd int) bool { return false }
