//go:build windows

package lockfile

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// noFollow is 0: there is no O_NOFOLLOW outside Unix. Accepted: Windows isn't a supported
// server platform (Linux and macOS are), it only has to build, and creating a symlink there
// takes privileges an attacker on the data directory wouldn't have to begin with.
const noFollow = 0

// lock takes an exclusive lock on f without waiting, or returns ErrLocked. It locks one
// byte far past the end of the file, so others can still read the PID at its start.
func lock(f *os.File) error {
	ol := &windows.Overlapped{OffsetHigh: 0x7fffffff}
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrLocked
	}
	return err
}
