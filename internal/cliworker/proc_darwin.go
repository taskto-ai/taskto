//go:build darwin

package cliworker

import (
	"errors"
	"fmt"
	"math"
	"syscall"

	"golang.org/x/sys/unix"
)

func dieWithParent(*syscall.SysProcAttr) {}

func startTime(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if pid < 0 || pid > math.MaxInt32 || kp.Proc.P_pid != int32(pid) {
		return "", fmt.Errorf("no process %d", pid)
	}
	t := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", t.Sec, t.Usec), nil
}

func waitLeader(pid int) bool {
	if pid <= 0 {
		return false
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return false
	}
	defer unix.Close(kq)
	ev := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	out := make([]unix.Kevent_t, 1)
	for {
		_, err := unix.Kevent(kq, []unix.Kevent_t{ev}, out, nil)
		switch {
		case err == nil:
			return true
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ESRCH):
			return true
		default:
			return false
		}
	}
}
