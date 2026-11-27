//go:build !linux

package cliworker

import (
	"errors"
	"os/exec"
	"time"
)

const isolationSupported = false

type Isolation struct {
	UIDBase         int
	UserFile, Group string
}

func (w *Worker) asUser(_ *exec.Cmd, _ int64) error {
	if w.cfg.Isolation != nil {
		return errors.New("a Unix user per taskto user needs Linux")
	}
	return nil
}

func killUIDs(int) (int, error) { return 0, nil }

func (w *Worker) dropUnixUser(int64) {}

func killUID(int, time.Duration) error { return nil }

func checkPidfds() error { return nil }
