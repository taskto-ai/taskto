//go:build unix

package cliworker

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

const supported = true

func newGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	dieWithParent(cmd.SysProcAttr)
}

func signalGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 1 {
		return errors.New("refusing to signal an invalid process group")
	}
	err := syscall.Kill(-pgid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func groupExists(pgid int) bool {
	return !errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

const (
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
)

func execve(bin string, argv, env []string) error {
	return syscall.Exec(bin, argv, env)
}

func closeStdin() error {
	f, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer f.Close()
	return dup2(int(f.Fd()), 0)
}

func setUmask(mask int) { syscall.Umask(mask) }
