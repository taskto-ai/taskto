//go:build linux

package cliworker

import "golang.org/x/sys/unix"

func dup2(from, to int) error {
	if from == to {
		return nil
	}
	return unix.Dup3(from, to, 0)
}
