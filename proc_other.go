//go:build !windows

package main

import "os/exec"

func killChildrenOnExit() {}

func hideWindow(cmd *exec.Cmd) {}

func startDetached(cmd *exec.Cmd) error { return cmd.Start() }
