//go:build linux

package cliworker

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const isolationSupported = true

// isolationState is the users the worker has made so far.
type isolationState struct {
	mu    sync.Mutex
	known map[int64]bool
}

// asUser makes cmd run as the Unix user of the account with home number home, creating it first.
// Without isolation it does nothing.
func (w *Worker) asUser(cmd *exec.Cmd, home int64) error {
	iso := w.cfg.Isolation
	if iso == nil {
		return nil
	}
	uid, err := iso.uidOf(home)
	if err != nil {
		return err
	}
	if err := w.ensureUser(home, uid); err != nil {
		return err
	}
	id := uint32(uid) //nolint:gosec // below maxUID: uidOf, and New's check of UIDBase
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: id, Gid: id, Groups: []uint32{}}
	return nil
}

// ensureUser adds home's Unix user and group if they aren't there, and creates its directory,
// owned by it. It refuses a directory that is a symlink or someone else's.
func (w *Worker) ensureUser(home int64, uid int) error {
	iso := w.cfg.Isolation
	iso.state.mu.Lock()
	defer iso.state.mu.Unlock()
	if iso.state.known[home] {
		return nil
	}
	dirs := w.cfg.Layout.Home(home)
	name := "th" + strconv.FormatInt(home, 10)
	if err := addLine(iso.UserFile, name, uid, fmt.Sprintf("%s:x:%d:%d:taskto account:%s:/bin/sh", name, uid, uid, dirs.Home)); err != nil {
		return fmt.Errorf("adding the Unix user: %w", err)
	}
	if err := addLine(iso.Group, name, uid, fmt.Sprintf("%s:x:%d:", name, uid)); err != nil {
		return fmt.Errorf("adding the Unix group: %w", err)
	}
	info, err := os.Lstat(dirs.Root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(dirs.Root, 0o700); err != nil {
			return err
		}
		if err := os.Lchown(dirs.Root, uid, uid); err != nil {
			return err
		}
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("%s is not a directory", dirs.Root)
	default:
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) != uid {
			return fmt.Errorf("%s belongs to someone else", dirs.Root)
		}
	}
	if iso.state.known == nil {
		iso.state.known = map[int64]bool{}
	}
	iso.state.known[home] = true
	return nil
}

// dropUnixUser lets ensureUser make home's directory again, after it was removed: its Unix user
// stays, with the same uid.
func (w *Worker) dropUnixUser(home int64) {
	iso := w.cfg.Isolation
	if iso == nil {
		return
	}
	iso.state.mu.Lock()
	defer iso.state.mu.Unlock()
	delete(iso.state.known, home)
}

// checkPidfds makes sure this kernel and its system-call policy give the worker pidfds, which it
// ends an account's processes with: a pid read earlier may be another process's by the time it
// is signalled, a pidfd never.
func checkPidfds() error {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return fmt.Errorf("pidfd_open isn't allowed here (Linux 5.3 or later is needed, and a seccomp profile that allows it): %w", err)
	}
	defer unix.Close(fd)
	if err := unix.PidfdSendSignal(fd, 0, nil, 0); err != nil {
		return fmt.Errorf("pidfd_send_signal isn't allowed here (Linux 5.3 or later is needed, and a seccomp profile that allows it): %w", err)
	}
	return nil
}

// procRoot is where the processes are listed (a test's own, in tests).
var procRoot = "/proc"

// killUID kills every process of uid and waits until none is left (a zombie, killed and waiting to
// be reaped, holds nothing and doesn't count), for at most wait: a process a CLI detached into a
// group or session of its own is one of them. ErrBusy if one is still there after it. Only a full
// look at the processes says none is left: one that can't be made, or a process whose state can't
// be read, is an error.
func killUID(uid int, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		pids, err := uidProcesses(uid)
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}
		for _, pid := range pids {
			if _, err := killIf(pid, func(owner int) bool { return owner == uid }); err != nil {
				return err
			}
		}
		if time.Now().After(deadline) {
			return ErrBusy
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// uidProcesses lists the processes whose real uid is uid, apart from zombies, PID 1 and the worker.
func uidProcesses(uid int) ([]int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("listing the processes: %w", err)
	}
	self := os.Getpid()
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || pid == self {
			continue
		}
		owner, zombie, err := procStatus(pid)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue // it ended
		}
		if err != nil {
			return nil, fmt.Errorf("reading process %d: %w", pid, err)
		}
		if owner == uid && !zombie {
			out = append(out, pid)
		}
	}
	return out, nil
}

// procStatus is pid's real uid and whether it is a zombie: a zombie whose other threads still run
// (its leader exited first) counts as running, as it is.
func procStatus(pid int) (uid int, zombie bool, err error) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, false, err
	}
	uid = -1
	threads := 0
	defer func() {
		if zombie && threads > 1 {
			zombie = false
		}
	}()
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "State:"); ok {
			zombie = strings.HasPrefix(strings.TrimSpace(rest), "Z")
		}
		if rest, ok := strings.CutPrefix(line, "Threads:"); ok {
			threads, _ = strconv.Atoi(strings.TrimSpace(rest))
		}
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			if fields := strings.Fields(rest); len(fields) > 0 {
				if uid, err = strconv.Atoi(fields[0]); err != nil {
					return 0, false, fmt.Errorf("a status with no uid: %w", err)
				}
			}
		}
	}
	if uid < 0 {
		return 0, false, errors.New("a status with no uid")
	}
	return uid, zombie, nil
}

// killIf kills pid only while it is a process whose uid mine says yes to, and reports whether it
// sent the signal. It takes a pidfd, a reference to the
// process that a pid given again can't take over, reads the uid, and checks through the reference
// that the process still runs (a pid isn't given again while its process runs, so what was read is
// that process's) before it signals it through the reference. A worker that isolates accounts
// starts only where pidfds work (checkPidfds).
func killIf(pid int, mine func(uid int) bool) (bool, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	switch {
	case errors.Is(err, unix.ESRCH):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("opening process %d: %w", pid, err)
	}
	defer unix.Close(fd)
	owner, zombie, err := procStatus(pid)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading process %d: %w", pid, err)
	}
	if err := unix.PidfdSendSignal(fd, 0, nil, 0); errors.Is(err, unix.ESRCH) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("checking process %d: %w", pid, err)
	}
	if !mine(owner) || zombie {
		return false, nil
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); errors.Is(err, unix.ESRCH) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("killing process %d: %w", pid, err)
	}
	return true, nil
}

// addLine appends line to the account file path unless the entry is there: one with name and
// id. An entry with the name and another id, or the id and another name, is refused rather
// than duplicated. The file is replaced in one rename. (Each first-time user rewrites the
// file: fine for the few hundred users a server has; the worker remembers whom it made.)
func addLine(path, name string, id int, line string) error {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the configured account file
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 3 {
			continue
		}
		sameName, sameID := fields[0] == name, fields[2] == strconv.Itoa(id)
		switch {
		case sameName && sameID:
			return nil
		case sameName || sameID:
			return fmt.Errorf("%s already has %s or %d for someone else", path, name, id)
		}
	}
	text := string(b)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".taskto-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(text + line + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil { //nolint:gosec // G302: account files are world-readable
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// killUIDs kills every process whose real uid is min or more: all the CLI users'. Each through a
// pidfd, as killUID does, so a pid given again meanwhile is never signalled. A list of the
// processes, or a process's state, that can't be read is an error: the sweep didn't see them all.
func killUIDs(min int) (int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, fmt.Errorf("listing the processes: %w", err)
	}
	self := os.Getpid()
	killed := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || pid == self {
			continue
		}
		uid, zombie, err := procStatus(pid)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return killed, fmt.Errorf("reading process %d: %w", pid, err)
		}
		if uid < min || zombie {
			continue
		}
		hit, err := killIf(pid, func(uid int) bool { return uid >= min })
		if err != nil {
			return killed, err
		}
		if hit {
			killed++
		}
	}
	return killed, nil
}
