//go:build !linux

package cliworker

import (
	"errors"
	"os/exec"
)

const isolationSupported = false

type Isolation struct {
	UIDBase       int
	Passwd, Group string
}

func (w *Worker) asUser(_ *exec.Cmd, _ int64) error {
	if w.cfg.Isolation != nil {
		return errors.New("a Unix user per taskto user needs Linux")
	}
	return nil
}

func killUIDs(int) int { return 0 }
