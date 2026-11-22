//go:build unix

package lockfile

import (
	"errors"
	"os"
	"syscall"
)

// noFollow makes opening a symlink in place of the lock file fail.
const noFollow = syscall.O_NOFOLLOW

// lock takes an exclusive flock on f without waiting, or returns ErrLocked.
func lock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // G115: a file descriptor fits in an int
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}
