package cliworker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (w *Worker) removeAccount(ctx context.Context, acct account) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b := w.browserStopper(); b != nil {
		if err := b.StopAll(ctx, acct.public()); err != nil {
			return fmt.Errorf("stopping the account's browsers: %w", err)
		}
	}
	if err := w.quiesce(ctx, scope{id: acct.id, all: true}); err != nil {
		return err
	}
	if iso := w.cfg.Isolation; iso != nil {
		uid, err := iso.uidOf(acct.home)
		if err != nil {
			return err
		}
		if err := killUID(uid, uidWait); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.dropUnixUser(acct.home)
	name := "h" + strconv.FormatInt(acct.home, 10)
	trash, err := w.detach(w.cfg.Layout.Home(acct.home).Root, name)
	if err != nil {
		return fmt.Errorf("taking the account's directory away: %w", err)
	}
	w.dropState(acct.id)
	if trash != "" {
		if err := removeTree(trash, w.cfg.Isolation != nil); err != nil {
			return fmt.Errorf("removing the account's old directory: %w", err)
		}
	}
	return w.removeLeft(name)
}

func (w *Worker) removeLeft(name string) error {
	for _, pattern := range []string{
		filepath.Join(w.cfg.Layout.Root, detachedPrefix+name+"-*"),
		filepath.Join(w.trashDir(), name+"-*"),
	} {
		left, err := filepath.Glob(pattern)
		if err != nil {
			return err
		}
		for _, p := range left {
			if err := removeTree(p, w.cfg.Isolation != nil); err != nil {
				return fmt.Errorf("removing what an earlier removal left: %w", err)
			}
		}
	}
	return nil
}

const uidWait = 5 * time.Second

func (w *Worker) dropState(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k := range w.logins {
		if k.id == id {
			delete(w.logins, k)
		}
	}
	for tid, t := range w.turns {
		if t.acct.id == id {
			delete(w.turns, tid)
		}
	}
}

func (w *Worker) trashDir() string { return filepath.Join(w.stateDir(), "trash") }

func (w *Worker) detach(root, name string) (string, error) {
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(w.trashDir(), 0o700); err != nil {
		return "", err
	}
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", err
	}
	leaf := name + "-" + hex.EncodeToString(r[:])
	aside := filepath.Join(filepath.Dir(root), detachedPrefix+leaf)
	if err := os.Rename(root, aside); err != nil {
		return "", err
	}
	if err := syncDir(filepath.Dir(root)); err != nil {
		return "", err
	}
	if hook := w.detachHookForTest; hook != nil {
		if err := hook(); err != nil {
			return "", err
		}
	}
	if info.IsDir() {
		if err := takeOver(aside, w.cfg.Isolation != nil); err != nil {
			return "", err
		}
	}
	trash := filepath.Join(w.trashDir(), leaf)
	if err := os.Rename(aside, trash); err != nil {
		return "", err
	}
	if err := syncDir(filepath.Dir(root)); err != nil {
		return "", err
	}
	return trash, nil
}

const detachedPrefix = ".detached-"

func (w *Worker) sweepDetached() error {
	left, _ := filepath.Glob(filepath.Join(w.cfg.Layout.Root, detachedPrefix+"*"))
	for _, p := range left {
		if err := removeTree(p, w.cfg.Isolation != nil); err != nil {
			return fmt.Errorf("cliworker: removing what a removal left: %w", err)
		}
	}
	return nil
}

func takeOver(path string, chown bool) error {
	if chown {
		if err := os.Lchown(path, os.Getuid(), os.Getgid()); err != nil {
			return err
		}
	}
	return os.Chmod(path, 0o700)
}

func removeTree(path string, chown bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return os.Remove(path)
	}
	if err := takeOver(path, chown); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := removeTree(filepath.Join(path, e.Name()), chown); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func (w *Worker) sweepTrash() error {
	entries, err := os.ReadDir(w.trashDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if err := removeTree(filepath.Join(w.trashDir(), e.Name()), w.cfg.Isolation != nil); err != nil {
			errs = append(errs, fmt.Errorf("removing %s from the trash: %w", e.Name(), err))
		}
	}
	return errors.Join(errs...)
}

func (w *Worker) removeLegacy(user int64) error {
	name := "u" + strconv.FormatInt(user, 10)
	trash, err := w.detach(w.cfg.Layout.legacy(user), name)
	if err != nil {
		return err
	}
	if trash != "" {
		if err := removeTree(trash, w.cfg.Isolation != nil); err != nil {
			return err
		}
	}
	return w.removeLeft(name)
}

func (w *Worker) checkLegacyOwners(base int) error {
	users, err := w.legacyDirs()
	if err != nil {
		return err
	}
	lo, hi := int64(base)+firstHome, int64(base)+maxHome
	for _, user := range users {
		fi, err := os.Lstat(w.cfg.Layout.legacy(user))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("cliworker: can't tell who owns the directories from before homes")
		}
		if uid := int64(st.Uid); uid >= lo && uid < hi {
			return fmt.Errorf("cliworker: u%d, from before homes, belongs to uid %d, which -uid-base %d would give a new account: start the worker with the UID base it had before, or on a new volume", user, uid, base)
		}
	}
	return nil
}

func (w *Worker) legacyDirs() ([]int64, error) {
	entries, err := os.ReadDir(w.cfg.Layout.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, e := range entries {
		id, ok := strings.CutPrefix(e.Name(), "u")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(id, 10, 64); err == nil && n > 0 && strconv.FormatInt(n, 10) == id {
			out = append(out, n)
		}
	}
	return out, nil
}
