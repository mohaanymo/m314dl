//go:build !linux

package main

import "os/exec"

// hardenCmd is a no-op off Linux, where Pdeathsig isn't available. Graceful
// shutdown still stops children; only hard-crash orphan-reaping is Linux-only.
func hardenCmd(cmd *exec.Cmd) {}
