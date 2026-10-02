//go:build unix

package main

import (
	"syscall"
	"unsafe"
)

// Raw terminal mode via termios ioctls — the same code on every unix; only the
// request numbers differ (rawmode_linux.go / rawmode_bsd.go). No cgo, no deps.

// makeRaw puts the terminal (fd) into raw mode and returns a restore func. Errors
// when fd is not a terminal (piped/redirected), so the caller falls back to a
// cooked line read. Flips input flags only; restored on exit.
func makeRaw(fd int) (func(), error) {
	var old syscall.Termios
	if err := ioctlTermios(fd, termiosGet, &old); err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctlTermios(fd, termiosSet, &raw); err != nil {
		return nil, err
	}
	return func() { ioctlTermios(fd, termiosSet, &old) }, nil
}

// makeTurnMode is the terminal state while the agent works: no line editing
// and no echo, so a long paste isn't cut at the canonical line limit and
// type-ahead doesn't scribble over the agent's output — but signals stay on, so
// Ctrl-C still interrupts the turn.
func makeTurnMode(fd int) (func(), error) {
	var old syscall.Termios
	if err := ioctlTermios(fd, termiosGet, &old); err != nil {
		return nil, err
	}
	t := old
	t.Lflag &^= syscall.ICANON | syscall.ECHO
	t.Cc[syscall.VMIN] = 0
	t.Cc[syscall.VTIME] = 0
	if err := ioctlTermios(fd, termiosSet, &t); err != nil {
		return nil, err
	}
	return func() { ioctlTermios(fd, termiosSet, &old) }, nil
}

// isTerminal reports whether fd has a terminal behind it. The cheapest honest
// test there is: ask for the line discipline and see whether the kernel has one
// to give. Nothing is changed and nothing is read, so it is safe on a descriptor
// a prompt is about to use.
func isTerminal(fd int) bool {
	if fd < 0 {
		return false
	}
	var t syscall.Termios
	return ioctlTermios(fd, termiosGet, &t) == nil
}

func ioctlTermios(fd int, req uint, t *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}
