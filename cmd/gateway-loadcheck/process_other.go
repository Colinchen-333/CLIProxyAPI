//go:build !darwin && !linux

package main

import (
	"os"
	"os/exec"
)

func configureProcess(cmd *exec.Cmd) {}
func interruptProcess(cmd *exec.Cmd) { _ = cmd.Process.Signal(os.Interrupt) }
func killProcess(cmd *exec.Cmd)      { _ = cmd.Process.Kill() }
