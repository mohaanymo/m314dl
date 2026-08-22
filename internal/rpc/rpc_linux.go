package rpc

import (
	"os/exec"
	"syscall"
)

// hardenCmd makes a job's child process robust for an unattended server:
//   - Pdeathsig SIGKILL: if the RPC server dies (even by SIGKILL), the kernel
//     kills the child too, so a crash never leaves orphaned downloads running.
//   - Setpgid: the child leads its own process group, so anything it spawns
//     (e.g. ffmpeg) is contained and can be signalled as a unit.
func hardenCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
		Setpgid:   true,
	}
}
