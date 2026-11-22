//go:build linux

package cliworker

import "golang.org/x/sys/unix"

// noNewPrivs sets no_new_privs on the calling OS thread, which an exec from that thread keeps:
// from then on a setuid or setgid program (sudo, su) can't raise its privileges, or its
// children's. It is per thread, so the caller must stay on one thread until it execs
// (RunHelper locks it).
func noNewPrivs() error { return unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) }
