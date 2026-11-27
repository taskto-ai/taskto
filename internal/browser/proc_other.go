//go:build !unix

package browser

import (
	"errors"
	"os/exec"
)

type signal int

const (
	sigTerm signal = 15
	sigKill signal = 9
)

func NewGroup(*exec.Cmd) {}

func signalGroup(int, signal) error { return errors.New("browsers need a Unix system") }
