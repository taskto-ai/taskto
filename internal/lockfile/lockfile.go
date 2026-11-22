// Package lockfile holds an exclusive lock on a file for as long as a process runs: the
// data directory's (DataDir), so only one server, or one restore, uses it at a time, and
// the agent runtime's database's.
//
// The lock is a flock(2) on the open file, not the file's existence: the kernel releases
// it when the file is closed or the process exits, however it exits, so a lock file left
// behind by a crash is no obstacle. The file is opened close-on-exec, so child processes
// (the agent sidecar and the commands it runs) never inherit it and can't hold it after
// the process that took it has gone.
package lockfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DataDirName is the lock file in the data directory.
const DataDirName = "taskto.lock"

// ErrLocked is matched (errors.Is) by the error Lock returns when another open file holds
// the lock, in this process or another.
var ErrLocked = errors.New("locked by another process")

// HeldError is the error Lock returns when the lock is held.
type HeldError struct {
	Path string
	// PID is the process ID the holder wrote into the lock file, or 0 if it hasn't yet.
	PID int
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("%s is locked by another process (%s)", e.Path, e.holder())
}

func (e *HeldError) holder() string {
	if e.PID == 0 {
		return "pid unknown"
	}
	return "pid " + strconv.Itoa(e.PID)
}

// dataDirHeldError is DataDir's error when another process holds the lock: a message for the
// operator, still matching errors.Is(err, ErrLocked) and errors.As(err, **HeldError).
type dataDirHeldError struct {
	dir  string
	held *HeldError
}

func (e *dataDirHeldError) Error() string {
	return fmt.Sprintf("data directory %s is in use by another taskto process (%s): stop it first, or use another data directory", e.dir, e.held.holder())
}

func (e *dataDirHeldError) Unwrap() error { return e.held }

// Is makes errors.Is(err, ErrLocked) true.
func (e *HeldError) Is(target error) bool { return target == ErrLocked }

// Lock creates path if needed and takes an exclusive lock on it without waiting, then
// writes this process's ID into it. The lock is held until the returned file is closed or
// the process exits. If another holds it, Lock returns a *HeldError naming its PID. A
// symbolic link in place of path is not followed.
func Lock(path string) (*os.File, error) {
	// The file can be removed and made again between opening and locking it, by another
	// process doing the same: then this lock is on a file nobody else will find. Locking
	// again on what the path names now settles it; it is rare, so a few tries do.
	for range 5 {
		f, err := lockOnce(path)
		if !errors.Is(err, errReplaced) {
			return f, err
		}
	}
	return nil, fmt.Errorf("lock %s: the file keeps being replaced", path)
}

// errReplaced is for a lock file replaced while it was being locked.
var errReplaced = errors.New("lock file replaced")

// afterOpen, when set (by tests), runs between opening the lock file and locking it.
var afterOpen func()

func lockOnce(path string) (*os.File, error) {
	// os.OpenFile opens close-on-exec. No O_TRUNC: if another holds the lock, its PID stays.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|noFollow, 0o600) //nolint:gosec // G304: the path comes from the server's config
	if err != nil {
		return nil, err
	}
	if afterOpen != nil {
		afterOpen()
	}
	if err := lock(f); err != nil {
		if errors.Is(err, ErrLocked) {
			held := &HeldError{Path: path, PID: holder(f)}
			f.Close()
			return nil, held
		}
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// Still the file the path names? If not, this lock guards nothing.
	locked, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if now, err := os.Lstat(path); err != nil || !os.SameFile(locked, now) { //nolint:gosec // G703: the path comes from the server's config, as above
		f.Close()
		return nil, errReplaced
	}
	// Only the holder writes, so the PID in the file is the holder's, or a crashed
	// holder's until this write.
	pid := []byte(strconv.Itoa(os.Getpid()) + "\n")
	err = f.Truncate(0)
	if err == nil {
		_, err = f.WriteAt(pid, 0)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}

// holder returns the PID written in the lock file f, read through f itself (the very file
// whose lock is held, not whatever the path names now), or 0 if there is none yet.
func holder(f *os.File) int {
	b := make([]byte, 32)
	n, err := f.ReadAt(b, 0)
	if n == 0 && err != nil {
		return 0
	}
	b = b[:n]
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// DataDir locks the data directory dir, which must exist, for the whole of a server's or
// a restore's run: exactly one process uses a data directory at a time. Close the file
// to release it.
func DataDir(dir string) (*os.File, error) {
	f, err := Lock(filepath.Join(dir, DataDirName))
	if held := (*HeldError)(nil); errors.As(err, &held) {
		return nil, &dataDirHeldError{dir: dir, held: held}
	}
	if err != nil {
		return nil, fmt.Errorf("data directory lock: %w", err)
	}
	return f, nil
}
