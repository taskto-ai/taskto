//go:build !linux

package cliworker

import (
	"context"
	"errors"
	"os/exec"
)

const isolationSupported = false

type isolationState struct{} //nolint:unused // isolation needs Linux

func (w *Worker) asUser(_ *exec.Cmd, _ int64) error {
	if w.cfg.Isolation != nil {
		return errors.New("a Unix user per taskto user needs Linux")
	}
	return nil
}

func killUIDs(int) int { return 0 }

func (w *Worker) dropUnixUser(int64) {}

func (w *Worker) endUserProcesses(context.Context, int64) error { return nil }
