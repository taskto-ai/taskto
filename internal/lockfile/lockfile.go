package lockfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const DataDirName = "taskto.lock"

var ErrLocked = errors.New("locked by another process")

type HeldError struct {
	Path string
	PID  int
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

type dataDirHeldError struct {
	dir  string
	held *HeldError
}

func (e *dataDirHeldError) Error() string {
	return fmt.Sprintf("data directory %s is in use by another taskto process (%s): stop it first, or use another data directory", e.dir, e.held.holder())
}

func (e *dataDirHeldError) Unwrap() error { return e.held }

func (e *HeldError) Is(target error) bool { return target == ErrLocked }

func Lock(path string) (*os.File, error) {
	for range 5 {
		f, err := lockOnce(path)
		if !errors.Is(err, errReplaced) {
			return f, err
		}
	}
	return nil, fmt.Errorf("lock %s: the file keeps being replaced", path)
}

var errReplaced = errors.New("lock file replaced")

var afterOpen func()

func lockOnce(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|noFollow, 0o600)
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
	locked, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if now, err := os.Lstat(path); err != nil || !os.SameFile(locked, now) {
		f.Close()
		return nil, errReplaced
	}
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
