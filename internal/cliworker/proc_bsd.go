//go:build unix && !linux && !darwin

package cliworker

import (
	"errors"
	"syscall"
)

func dieWithParent(*syscall.SysProcAttr) {}

// startTime isn't implemented here: without it, the start-up sweep can't tell a turn's
// process from a later one with the same PID, so it leaves both alone.
func startTime(int) (string, error) {
	return "", errors.New("process start times are not supported on this system")
}

// waitLeader can't wait without reaping here: it reports false, and the caller kills the
// group after reaping its leader.
func waitLeader(int) bool { return false }
