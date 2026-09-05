//go:build !windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"unsafe"
)

func enableVT() {}

type winsize struct{ Row, Col, X, Y uint16 }

// termSize uses the TIOCGWINSZ ioctl directly — no fork, no x/term.
func termSize() (rows, cols int) {
	var ws winsize
	r, _, _ := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if int(r) == -1 || ws.Row == 0 {
		return 24, 80
	}
	return int(ws.Row), int(ws.Col)
}

// notifyResize delivers SIGWINCH so the client never has to poll.
func notifyResize(ch chan<- os.Signal) { signal.Notify(ch, syscall.SIGWINCH) }

// echoOff / echoOn toggle terminal echo for password prompts. Using stty keeps the
// terminal in cooked mode (line editing still works); it is the only place the client
// touches terminal modes at all.
func echoOff() { stty("-echo") }
func echoOn()  { stty("echo") }

func stty(arg string) {
	cmd := exec.Command("stty", arg)
	cmd.Stdin = os.Stdin
	cmd.Run()
}
