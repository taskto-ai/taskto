//go:build linux

package cliworker

import "golang.org/x/sys/unix"

func noNewPrivs() error { return unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) }
