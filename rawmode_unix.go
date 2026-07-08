//go:build linux

package main

import (
	"syscall"
	"unsafe"
)

// makeRaw puts the terminal (fd) into raw mode and returns a restore func. Errors
// when fd is not a terminal (piped/redirected), so the caller falls back to a
// cooked line read. Read-only-ish: flips input flags, restored on exit. No cgo.
func makeRaw(fd int) (func(), error) {
	var old syscall.Termios
	if err := ioctlTermios(fd, syscall.TCGETS, &old); err != nil {
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
	if err := ioctlTermios(fd, syscall.TCSETS, &raw); err != nil {
		return nil, err
	}
	return func() { ioctlTermios(fd, syscall.TCSETS, &old) }, nil
}

// enterFooterRaw is like makeRaw but sets VMIN=0/VTIME=1 so reads return every
// ~100ms even with no input — letting the footer's reader poll a stop channel
// and exit promptly. Returns a restore func.
func enterFooterRaw(fd int) (func(), error) {
	var old syscall.Termios
	if err := ioctlTermios(fd, syscall.TCGETS, &old); err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 0
	raw.Cc[syscall.VTIME] = 1
	if err := ioctlTermios(fd, syscall.TCSETS, &raw); err != nil {
		return nil, err
	}
	return func() { ioctlTermios(fd, syscall.TCSETS, &old) }, nil
}

// readStdinByte reads one byte from stdin, returning ok=false on a VTIME timeout
// (no input) so the caller can re-check whether it should stop.
func readStdinByte() (byte, bool) {
	var b [1]byte
	n, err := syscall.Read(0, b[:])
	if n == 1 && err == nil {
		return b[0], true
	}
	return 0, false
}

func ioctlTermios(fd int, req uint, t *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}
