//go:build darwin

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// openPTY gives a test a real terminal on both ends. Terminal behaviour that
// only appears on a tty — a read that returns zero bytes under -icanon, echo,
// window size — cannot be reproduced with a pipe, and that is exactly where the
// bugs live.
func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	fd := m.Fd()
	if err := ioctl0(fd, syscall.TIOCPTYGRANT); err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("grant: %w", err)
	}
	if err := ioctl0(fd, syscall.TIOCPTYUNLK); err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("unlock: %w", err)
	}
	var name [128]byte
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); e != 0 {
		m.Close()
		return nil, nil, fmt.Errorf("name: %w", e)
	}
	n := 0
	for n < len(name) && name[n] != 0 {
		n++
	}
	s, err := os.OpenFile(string(name[:n]), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}

func ioctl0(fd, req uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, 0); e != 0 {
		return e
	}
	return nil
}
