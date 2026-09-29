//go:build unix

package main

import (
	"os"
	"syscall"
)

// readAvailable returns everything the terminal has ready without blocking.
// The fd goes non-blocking for the duration and is restored afterwards, so the
// editor's ordinary blocking reads keep working.
func readAvailable(f *os.File) []byte {
	fd := int(f.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		return nil
	}
	defer syscall.SetNonblock(fd, false)
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := syscall.Read(fd, buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if n <= 0 || err != nil {
			return out
		}
	}
}
