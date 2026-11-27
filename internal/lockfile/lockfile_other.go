//go:build !unix && !windows

package lockfile

import (
	"errors"
	"os"
)

const noFollow = 0

func lock(*os.File) error {
	return errors.New("locking needs Linux, macOS or another Unix system")
}
