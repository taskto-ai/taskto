//go:build linux

package cliworker

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"math"
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

type Isolation struct {
	UIDBase         int
	UserFile, Group string

	state isolationState
}

type isolationState struct {
	mu    sync.Mutex
	known map[int64]bool
}

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
	if uid < 0 || int64(uid) > math.MaxUint32 {
		return fmt.Errorf("home %d's Unix user id is out of range", home)
	}
	id := uint32(uid)
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: id, Gid: id, Groups: []uint32{}}
	return nil
}

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
		tmp, err := os.MkdirTemp(w.cfg.Layout.Root, provisionPrefix+strconv.FormatInt(home, 10)+"-")
		if err != nil {
			return err
		}
		if err := os.Lchown(tmp, uid, uid); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if hook := w.provisionHookForTest; hook != nil {
			if err := hook(); err != nil {
				return err
			}
		}
		if err := os.Rename(tmp, dirs.Root); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if err := syncDir(w.cfg.Layout.Root); err != nil {
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

func (w *Worker) dropUnixUser(home int64) {
	iso := w.cfg.Isolation
	if iso == nil {
		return
	}
	iso.state.mu.Lock()
	defer iso.state.mu.Unlock()
	delete(iso.state.known, home)
}

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

var procRoot = "/proc"

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
			continue
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

func addLine(path, name string, id int, line string) error {
	b, err := os.ReadFile(filepath.Clean(path))
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
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

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
