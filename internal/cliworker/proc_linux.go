//go:build linux

package cliworker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var procFS = os.DirFS("/proc")

func dieWithParent(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGKILL }

func startTime(pid int) (string, error) {
	b, err := fs.ReadFile(procFS, strconv.Itoa(pid)+"/stat")
	if err != nil {
		return "", err
	}
	s := string(b)
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return "", fmt.Errorf("unexpected /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 20 {
		return "", fmt.Errorf("unexpected /proc/%d/stat", pid)
	}
	return fields[19], nil
}

func waitLeader(pid int) bool {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err == nil
		}
	}
}

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
		b, err := fs.ReadFile(procFS, strconv.Itoa(pid)+"/stat")
		if err != nil {
			continue
		}
		if state, group, ok := parseStat(string(b)); ok && group == pgid && state != 'Z' && state != 'X' {
			return true
		}
	}
	return false
}

func parseStat(s string) (state byte, pgrp int, ok bool) {
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 3 || len(fields[0]) != 1 {
		return 0, 0, false
	}
	n, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], n, true
}
