//go:build windows

package main

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procSetConsoleOutputCP         = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleCP               = kernel32.NewProc("SetConsoleCP")
)

type coord struct{ X, Y int16 }
type smallRect struct{ Left, Top, Right, Bottom int16 }
type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

// enableVT turns on ANSI escape processing (already on in Windows Terminal,
// needed for legacy conhost) and switches input/output to UTF-8.
func enableVT() {
	const enableVirtualTerminalProcessing = 0x0004
	h, _ := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	var mode uint32
	procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode)))
	procSetConsoleMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing))
	procSetConsoleOutputCP.Call(65001)
	procSetConsoleCP.Call(65001)
}

func termSize() (rows, cols int) {
	h, _ := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	var info consoleScreenBufferInfo
	r, _, _ := procGetConsoleScreenBufferInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 24, 80
	}
	return int(info.Window.Bottom-info.Window.Top) + 1, int(info.Window.Right-info.Window.Left) + 1
}

// notifyResize: Windows has no SIGWINCH; poll the console size every 2s (a
// single kernel call, no process spawn) and signal only when it changes.
func notifyResize(ch chan<- os.Signal) {
	go func() {
		r, c := termSize()
		for {
			time.Sleep(2 * time.Second)
			nr, nc := termSize()
			if nr != r || nc != c {
				r, c = nr, nc
				ch <- syscall.SIGTERM // value is irrelevant; it's just a wake-up
			}
		}
	}()
}

// echoOff / echoOn toggle ENABLE_ECHO_INPUT on the console input handle for password
// prompts. Line input stays on, so Enter still delivers the whole line.
const enableEchoInput = 0x0004

func echoOff() { setEcho(false) }
func echoOn()  { setEcho(true) }

func setEcho(on bool) {
	h, _ := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE)
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return
	}
	if on {
		mode |= enableEchoInput
	} else {
		mode &^= enableEchoInput
	}
	procSetConsoleMode.Call(uintptr(h), uintptr(mode))
}
