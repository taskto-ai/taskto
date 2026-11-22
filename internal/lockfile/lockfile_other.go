//go:build !unix && !windows

package lockfile

import (
	"errors"
	"os"
)

// noFollow is 0: there is no O_NOFOLLOW outside Unix.
const noFollow = 0

// lock fails: without flock, nothing would keep a second server off the data directory.
func lock(*os.File) error {
	return errors.New("locking needs Linux, macOS or another Unix system")
}
