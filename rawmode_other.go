//go:build !linux

package main

import "errors"

// makeRaw is unsupported off unix; the caller falls back to a cooked line read.
func makeRaw(fd int) (func(), error) {
	return nil, errors.New("raw mode unsupported on this platform")
}

// enterFooterRaw / readStdinByte are unsupported off linux; the footer input
// line is simply disabled and the session behaves as before.
func enterFooterRaw(fd int) (func(), error) {
	return nil, errors.New("footer raw mode unsupported on this platform")
}

func readStdinByte() (byte, bool) { return 0, false }
