//go:build unix

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// osTermWidth returns the terminal's column count via a read-only TIOCGWINSZ
// ioctl on stdout. Read-only and self-contained (no cgo, no external deps).
// Returns 0 when stdout is not a terminal (piped/redirected), so the caller
// falls back to $COLUMNS / a default.
func osTermWidth() int {
	var ws struct{ rows, cols, x, y uint16 }
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		os.Stdout.Fd(),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno != 0 {
		return 0
	}
	return int(ws.cols)
}
