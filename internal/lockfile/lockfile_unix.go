//go:build unix

package lockfile

import (
	"errors"
	"os"
	"syscall"
)

const noFollow = syscall.O_NOFOLLOW

func lock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}
