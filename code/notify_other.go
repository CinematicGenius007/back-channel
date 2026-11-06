//go:build !windows

package main

import "os/exec"

func windowsToast(title, body string) *exec.Cmd { return nil }
