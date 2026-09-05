//go:build !windows

package main

// macOS and Linux clipboards through the standard helper commands. There is no cheap
// change counter without cgo (NSPasteboard.changeCount), so sync polls pbpaste/xclip
// every 1.5 s — only while /clip is on.

import (
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func clipPollInterval() time.Duration { return 1500 * time.Millisecond }

func clipSeq() uint32 { return 0 } // unknown: read and compare

func clipboardRead() (string, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbpaste")
	default:
		if _, err := exec.LookPath("wl-paste"); err == nil {
			cmd = exec.Command("wl-paste", "-n")
		} else {
			cmd = exec.Command("xclip", "-selection", "clipboard", "-o")
		}
	}
	out, err := cmd.Output()
	return string(out), err
}

func clipboardWrite(s string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	default:
		if _, err := exec.LookPath("wl-copy"); err == nil {
			cmd = exec.Command("wl-copy")
		} else {
			cmd = exec.Command("xclip", "-selection", "clipboard")
		}
	}
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}
