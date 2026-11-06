//go:build windows

package main

import (
	"errors"
	"syscall"
	"unsafe"
)

// Console input/output mode bits (consoleapi.h).
const (
	conProcessedInput = 0x0001
	conLineInput      = 0x0002
	conEchoInput      = 0x0004
	conQuickEdit      = 0x0040
	conExtendedFlags  = 0x0080
	conVTInput        = 0x0200
	conVTProcessing   = 0x0004
	conNoAutoReturn   = 0x0008
)

// rawMode switches the console to virtual-terminal input: keys, mouse/touch reports
// and bracketed pastes arrive as the same escape sequences a Unix terminal sends, so
// one parser serves every platform. Quick-edit is turned off, otherwise a click
// starts a text selection instead of reaching the app. Needs Windows 10 1809+;
// older consoles refuse the mode and the client falls back to the classic TUI.
func rawMode() (restore func(), err error) {
	in, _ := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE)
	out, _ := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	var inOld, outOld uint32
	if r, _, _ := procGetConsoleMode.Call(uintptr(in), uintptr(unsafe.Pointer(&inOld))); r == 0 {
		return nil, errors.New("stdin is not a console")
	}
	if r, _, _ := procGetConsoleMode.Call(uintptr(out), uintptr(unsafe.Pointer(&outOld))); r == 0 {
		return nil, errors.New("stdout is not a console")
	}
	inNew := (inOld &^ (conProcessedInput | conLineInput | conEchoInput | conQuickEdit)) | conVTInput | conExtendedFlags
	if r, _, _ := procSetConsoleMode.Call(uintptr(in), uintptr(inNew)); r == 0 {
		return nil, errors.New("this console does not support virtual-terminal input")
	}
	procSetConsoleMode.Call(uintptr(out), uintptr(outOld|conVTProcessing|conNoAutoReturn))
	procSetConsoleOutputCP.Call(65001)
	procSetConsoleCP.Call(65001)
	return func() {
		procSetConsoleMode.Call(uintptr(in), uintptr(inOld))
		procSetConsoleMode.Call(uintptr(out), uintptr(outOld))
	}, nil
}

func isTerminal() bool {
	var m uint32
	for _, std := range []int{syscall.STD_INPUT_HANDLE, syscall.STD_OUTPUT_HANDLE} {
		h, _ := syscall.GetStdHandle(std)
		if r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&m))); r == 0 {
			return false
		}
	}
	return true
}
