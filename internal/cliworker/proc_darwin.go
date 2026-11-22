//go:build darwin

package cliworker

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// dieWithParent has no equivalent on macOS: the start-up sweep stops orphans.
func dieWithParent(*syscall.SysProcAttr) {}

// startTime returns when process pid started: with the PID, it names one process, since a
// reused PID has a later start.
func startTime(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if kp.Proc.P_pid != int32(pid) { //nolint:gosec // a PID fits in 32 bits
		return "", fmt.Errorf("no process %d", pid)
	}
	t := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", t.Sec, t.Usec), nil
}

// waitLeader waits until process pid has exited, without reaping it (a kqueue NOTE_EXIT), and
// reports true: until it is reaped, its PID, and the ID of the process group it leads, can't
// be reused. It reports false if it can't watch the process, and the caller then kills the
// group after reaping its leader.
func waitLeader(pid int) bool {
	kq, err := unix.Kqueue()
	if err != nil {
		return false
	}
	defer unix.Close(kq)
	ev := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT} //nolint:gosec // a PID is positive
	out := make([]unix.Kevent_t, 1)
	for {
		_, err := unix.Kevent(kq, []unix.Kevent_t{ev}, out, nil)
		switch {
		case err == nil:
			return true
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ESRCH):
			// Exited already: only we reap it, so it is a zombie still.
			return true
		default:
			return false
		}
	}
}
