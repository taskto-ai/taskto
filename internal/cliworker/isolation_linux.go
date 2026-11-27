//go:build linux

package cliworker

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const isolationSupported = true

type Isolation struct {
	UIDBase       int
	Passwd, Group string

	state isolationState
}

type isolationState struct {
	mu    sync.Mutex
	known map[int64]bool
}

func (iso *Isolation) uid(user int64) int { return iso.UIDBase + int(user) }

const maxIsolatedUser = 1 << 30

func (w *Worker) asUser(cmd *exec.Cmd, user int64) error {
	iso := w.cfg.Isolation
	if iso == nil {
		return nil
	}
	if user <= 0 || user >= maxIsolatedUser {
		return fmt.Errorf("user %d can't have a Unix user", user)
	}
	if err := w.ensureUser(user); err != nil {
		return err
	}
	uid := iso.uid(user)
	if uid < 0 || int64(uid) > math.MaxUint32 {
		return fmt.Errorf("user %d's Unix user id is out of range", user)
	}
	id := uint32(uid)
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: id, Gid: id, Groups: []uint32{}}
	return nil
}

func (w *Worker) ensureUser(user int64) error {
	iso := w.cfg.Isolation
	iso.state.mu.Lock()
	defer iso.state.mu.Unlock()
	if iso.state.known[user] {
		return nil
	}
	uid := iso.uid(user)
	dirs := w.cfg.Layout.User(user)
	name := "tt" + strconv.FormatInt(user, 10)
	if err := addLine(iso.Passwd, name, uid, fmt.Sprintf("%s:x:%d:%d:TaskTo user %d:%s:/bin/sh", name, uid, uid, user, dirs.Home)); err != nil {
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
	iso.state.known[user] = true
	return nil
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

func killUIDs(min int) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	killed := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
				fields := strings.Fields(rest)
				if len(fields) > 0 {
					if uid, err := strconv.Atoi(fields[0]); err == nil && uid >= min {
						if syscall.Kill(pid, syscall.SIGKILL) == nil {
							killed++
						}
					}
				}
				break
			}
		}
	}
	return killed
}
