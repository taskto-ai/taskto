//go:build linux

package cliworker

import "golang.org/x/sys/unix"

// noNewPrivs sets no_new_privs on this process, which an exec keeps: from then on a setuid
// or setgid program (sudo, su) can't raise its privileges, or its children's.
func noNewPrivs() error { return unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) }
