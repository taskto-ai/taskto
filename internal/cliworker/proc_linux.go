//go:build linux

package cliworker

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// dieWithParent asks the kernel to kill the CLI if the thread that started it exits: a
// last resort when the worker dies without stopping its turns. The start-up sweep is the
// guarantee; this only shortens the time an orphan runs.
func dieWithParent(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGKILL }

// startTime returns when process pid started, in clock ticks since boot: with the PID,
// it names one process, since a reused PID has a later start.
func startTime(pid int) (string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") //nolint:gosec // G703: a number in a fixed /proc path
	if err != nil {
		return "", err
	}
	// The command name, field 2, is in parentheses and may hold spaces: count after it.
	s := string(b)
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return "", fmt.Errorf("unexpected /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[end+1:])
	// Field 22 overall is starttime; fields here start at field 3.
	if len(fields) < 20 {
		return "", fmt.Errorf("unexpected /proc/%d/stat", pid)
	}
	return fields[19], nil
}

// waitLeader waits until process pid has exited without reaping it (WNOWAIT), and reports
// true: until it is reaped, its PID, and the ID of the process group it leads, can't be
// reused, so its group can still be signalled safely.
func waitLeader(pid int) bool {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err == nil
		}
	}
}
