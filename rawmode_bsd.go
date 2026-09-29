//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package main

import "syscall"

// BSD-derived systems (macOS included) name the termios ioctls differently.
const (
	termiosGet = syscall.TIOCGETA
	termiosSet = syscall.TIOCSETA
)
