//go:build unix

package dictation

import (
	"errors"
	"os/exec"
	"syscall"
)

const groupsSupported = true

// newGroup makes cmd the leader of a process group of its own, killed whole when its
// context ends, so nothing a runtime starts outlives it.
func newGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil || cmd.Process.Pid <= 1 {
			return errors.New("no process to stop")
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
}
