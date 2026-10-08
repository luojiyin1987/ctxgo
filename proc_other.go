//go:build !linux

package main

import "os/exec"

func configureProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func signalNumber(*exec.ExitError) int { return 1 }
