//go:build !windows

package main

import (
	"os"
	"os/exec"
)

func killChildrenOnExit() {}

func killTree(p *os.Process) { _ = p.Kill() }

func hideWindow(cmd *exec.Cmd) {}

func startDetached(cmd *exec.Cmd) error { return cmd.Start() }
