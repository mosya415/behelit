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
