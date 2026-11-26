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

// groupLive reports whether a process of group pgid can still run: one that exists and isn't a
// zombie. A zombie is a process that was killed and waits for its parent to reap it (in a
// container without an init process, no one does): it holds no file and writes nothing, and
// signal 0 can't tell it from a live process, so each member's state is read in /proc. Where
// that can't be read it says true: the caller keeps what it was to remove.
func groupLive(pgid int) bool {
	if pgid <= 1 || !groupExists(pgid) {
		return false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") //nolint:gosec // G703: a number in a fixed /proc path
		if err != nil {
			continue // gone since the listing
		}
		if state, group, ok := parseStat(string(b)); ok && group == pgid && state != 'Z' && state != 'X' {
			return true
		}
	}
	return false
}

// parseStat reads the state and the process group out of the contents of /proc/<pid>/stat.
func parseStat(s string) (state byte, pgrp int, ok bool) {
	// The command name, field 2, is in parentheses and may hold spaces and parentheses.
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(s[end+1:]) // state, ppid, pgrp, …
	if len(fields) < 3 || len(fields[0]) != 1 {
		return 0, 0, false
	}
	n, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], n, true
}
