//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package main

import "errors"

// Platforms without a known termios ioctl fall back to the classic line-mode TUI.
func rawMode() (func(), error) { return nil, errors.New("raw mode not supported on this platform") }

func isTerminal() bool { return false }
