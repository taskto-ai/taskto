//go:build unix

package browser

import (
	"os/exec"
	"syscall"
)

type signal = syscall.Signal

const (
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
)

// NewGroup makes cmd's process the leader of a process group of its own, so the program and
// what it starts are signalled at once, and (Linux) killed if the thread that started it
// exits.
func NewGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	dieWithParent(cmd.SysProcAttr)
}

func signalGroup(pgid int, sig signal) error {
	if pgid <= 1 {
		return syscall.EINVAL
	}
	return syscall.Kill(-pgid, sig)
}
