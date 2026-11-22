//go:build linux

package cliworker

import "golang.org/x/sys/unix"

// dup2 is dup2(2): Dup3 refuses equal descriptors, which dup2 takes as a no-op.
func dup2(from, to int) error {
	if from == to {
		return nil
	}
	return unix.Dup3(from, to, 0)
}
