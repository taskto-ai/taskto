//go:build !unix

package cliworker

import "os/exec"

// CLI workers need process groups: Unix only.
const supported = false

type signal int

const (
	sigTerm signal = 15
	sigKill signal = 9
)

func newGroup(*exec.Cmd)            {}
func signalGroup(int, signal) error { return errUnsupported }
func startTime(int) (string, error) { return "", errUnsupported }

func waitLeader(int) bool { return false }

func groupAlive(int) bool { return false }

func execve(string, []string, []string) error { return errUnsupported }
func closeStdin() error                       { return errUnsupported }

func setUmask(int) {}
