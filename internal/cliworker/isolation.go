package cliworker

import "fmt"

// minUIDBase is the lowest first uid the worker accepts.
const minUIDBase = 65536

// Isolation runs each taskto user's CLIs as a Unix user of their own: the worker's container
// mode (docs/cli-workers.md). It needs Linux and a worker running as root (with CAP_SETUID,
// CAP_SETGID, CAP_CHOWN and CAP_KILL at least).
//
// User N runs as uid and gid UIDBase+N, with no supplementary groups, added to /etc/passwd and
// /etc/group the first time (Node's os.userInfo() needs an entry). Their directory under the
// layout's root is created by the worker, owned by them, mode 700; everything inside it is
// made by their own processes (the helper), never by the worker. The layout's root is the
// worker's (mode 711): users can reach their own directory and list nothing.
//
// When the worker starts it kills every process whose uid is UIDBase or more: nothing of a
// user's can be running legitimately then, and a uid, unlike a PID, is never reused by
// something else.
type Isolation struct {
	// UIDBase is the first uid (default 100000).
	UIDBase int
	// UserFile and Group are the account files (default /etc/passwd and /etc/group).
	UserFile, Group string

	state isolationState //nolint:unused // the users made so far: Linux only (isolation_linux.go)
}

// maxIsolatedUser bounds user IDs so uids stay below the 32-bit nobody range.
const maxIsolatedUser = 1 << 30

// uidOf is user's Unix user: UIDBase plus their number, for a number that can have one.
func (i *Isolation) uidOf(user int64) (int, error) {
	if user <= 0 || user >= maxIsolatedUser {
		return 0, fmt.Errorf("user %d can't have a Unix user", user)
	}
	return i.UIDBase + int(user), nil
}
