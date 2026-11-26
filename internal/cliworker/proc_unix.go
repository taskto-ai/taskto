//go:build unix

package cliworker

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

const supported = true

// newGroup makes cmd's process the leader of a process group of its own, so the CLI and
// everything it starts can be signalled at once.
func newGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	dieWithParent(cmd.SysProcAttr)
}

// signalGroup sends sig to every process in group pgid. A group that is gone is no error.
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

// groupAlive reports whether any process of group pgid still exists. A process that was
// killed and not yet reaped counts until its parent reaps it.
func groupAlive(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	return !errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

const (
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
)

// execve replaces this process with bin.
func execve(bin string, argv, env []string) error {
	return syscall.Exec(bin, argv, env) //nolint:gosec // G204: the configured CLI, run by the helper as its user
}

// closeStdin points stdin at /dev/null, so the CLI exec'd next sees it closed.
func closeStdin() error {
	f, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer f.Close()
	return dup2(int(f.Fd()), 0)
}

// setUmask sets the helper's umask before it execs a browser's program.
func setUmask(mask int) { syscall.Umask(mask) }
