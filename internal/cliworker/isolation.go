package cliworker

import "fmt"

// minUIDBase is the lowest first uid the worker accepts.
const minUIDBase = 65536

// Isolation runs each account's CLIs as a Unix user of its own: the worker's container mode
// (docs/cli-workers.md). It needs Linux and a worker running as root (with CAP_SETUID,
// CAP_SETGID, CAP_CHOWN and CAP_KILL at least).
//
// The account with home number H (accounts.go) runs as uid and gid UIDBase+H, with no
// supplementary groups, added to /etc/passwd and /etc/group the first time (Node's
// os.userInfo() needs an entry). A home number is never given to another account, so neither is
// its uid: what an account that is gone left behind (a process, a temporary file, a socket)
// belongs to a uid no later account has. Its directory under the layout's root is created by the
// worker, owned by it, mode 700; everything inside it is made by its own processes (the helper),
// never by the worker. The layout's root is the worker's (mode 711): an account can reach its own
// directory and list nothing.
//
// When the worker starts it kills every process whose uid is UIDBase or more: nothing of an
// account's can be running legitimately then, and a uid, unlike a PID, is never reused by
// something else. The kills go through pidfds, which the worker needs (Linux 5.3).
type Isolation struct {
	// UIDBase is the first uid (default 100000).
	UIDBase int
	// UserFile and Group are the account files (default /etc/passwd and /etc/group).
	UserFile, Group string

	state isolationState //nolint:unused // the users made so far: Linux only (isolation_linux.go)
}

// maxUID is the 32-bit nobody's uid: every account's uid is below it (New checks UIDBase).
const maxUID = 1<<32 - 2

// maxUIDBase is the highest first uid: UIDBase plus every home number stays below maxUID.
const maxUIDBase = maxUID - maxHome

// uidOf is the Unix user of the account with home number home: UIDBase plus the home number, which
// is the account's own and never another's (accounts.go). Starting an account's processes and
// ending them both use it.
func (i *Isolation) uidOf(home int64) (int, error) {
	if home < firstHome || home >= maxHome {
		return 0, fmt.Errorf("home %d can't have a Unix user", home)
	}
	return i.UIDBase + int(home), nil
}

// checkUIDBase is New's check of the first uid, made once, before any addition: above every system
// and image uid (nobody is 65534), since the start-up sweep kills every process from it up; and low
// enough that every account's uid (uidOf) is a valid one below the 32-bit nobody.
func checkUIDBase(base int) error {
	if base < minUIDBase {
		return fmt.Errorf("cliworker: the first uid must be %d or more", minUIDBase)
	}
	if base > maxUIDBase {
		return fmt.Errorf("cliworker: the first uid must be %d or less", maxUIDBase)
	}
	return nil
}
