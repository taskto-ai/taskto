//go:build unix && !linux && !darwin

package cliworker

import (
	"errors"
	"syscall"
)

func dieWithParent(*syscall.SysProcAttr) {}

func startTime(int) (string, error) {
	return "", errors.New("process start times are not supported on this system")
}

func waitLeader(int) bool { return false }
