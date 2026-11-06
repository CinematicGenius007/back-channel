//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// rawMode puts the terminal on stdin into raw mode (no line buffering, no echo, no
// signal keys) so the app TUI can read single keys, mouse/touch reports and bracketed
// pastes. It returns a function that restores the previous settings exactly.
//
// This is cfmakeraw(3) done by hand with the TCGETS/TIOCGETA ioctl, so it stays
// stdlib-only; output post-processing (OPOST) is left on so a stray "\n" still
// returns the carriage.
func rawMode() (restore func(), err error) {
	fd := os.Stdin.Fd()
	var old syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlGetTermios, uintptr(unsafe.Pointer(&old))); e != 0 {
		return nil, errors.New("stdin is not a terminal")
	}
	t := old
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlSetTermios, uintptr(unsafe.Pointer(&t))); e != 0 {
		return nil, errors.New("could not switch the terminal to raw mode")
	}
	return func() {
		syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlSetTermios, uintptr(unsafe.Pointer(&old)))
	}, nil
}

// isTerminal reports whether both stdin and stdout are terminals.
func isTerminal() bool {
	var t syscall.Termios
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ioctlGetTermios, uintptr(unsafe.Pointer(&t))); e != 0 {
			return false
		}
	}
	return true
}
