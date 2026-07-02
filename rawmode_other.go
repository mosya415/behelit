//go:build !linux

package main

import "errors"

// makeRaw is unsupported off unix; the caller falls back to a cooked line read.
func makeRaw(fd int) (func(), error) {
	return nil, errors.New("raw mode unsupported on this platform")
}
