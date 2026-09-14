//go:build !windows

package mtr

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}
