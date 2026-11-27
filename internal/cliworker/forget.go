package cliworker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (w *Worker) forgottenMark(user int64) string {
	return filepath.Join(w.stateDir(), "forgotten-u"+strconv.FormatInt(user, 10))
}

func (w *Worker) isForgotten(user int64) bool {
	_, err := os.Stat(w.forgottenMark(user))
	return !errors.Is(err, fs.ErrNotExist)
}

func (w *Worker) markForgotten(user int64) error {
	w.marks.Lock()
	defer w.marks.Unlock()
	if err := os.MkdirAll(w.stateDir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(w.forgottenMark(user), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if now := time.Now(); os.Chtimes(w.forgottenMark(user), now, now) != nil {
		_ = f.Close()
		return errors.New("recording the time of the mark")
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	d, err := os.Open(w.stateDir())
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return err
	}
	root, err := os.Open(w.cfg.Layout.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Sync()
}

func (w *Worker) Forget(ctx context.Context, user int64) error {
	if user <= 0 {
		return errors.New("invalid user")
	}
	sc := scope{user: user, all: true}
	end, err := w.beginGate(ctx, sc)
	if err != nil {
		return err
	}
	defer end()
	if err := w.markForgotten(user); err != nil {
		return fmt.Errorf("recording that the user was forgotten: %w", err)
	}
	if err := w.quiesce(ctx, sc); err != nil {
		return err
	}
	_, _, err = w.runHelper(ctx, user, helperRequest{Op: "forget"})
	return err
}

func (w *Worker) Reconcile(_ context.Context, live []int64, next int64, before time.Time) error {
	entries, err := os.ReadDir(w.stateDir())
	if err != nil {
		return err
	}
	isLive := make(map[int64]bool, len(live))
	for _, id := range live {
		isLive[id] = true
	}
	var errs []error
	for _, e := range entries {
		id, ok := strings.CutPrefix(e.Name(), "forgotten-u")
		if !ok {
			continue
		}
		user, err := strconv.ParseInt(id, 10, 64)
		if err != nil || user <= 0 || (!isLive[user] && user < next) {
			continue
		}
		if err := w.removeOldMark(e, before); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (w *Worker) removeOldMark(e fs.DirEntry, before time.Time) error {
	w.marks.Lock()
	defer w.marks.Unlock()
	info, err := os.Lstat(filepath.Join(w.stateDir(), e.Name()))
	if err != nil || !info.ModTime().Before(before) {
		return nil
	}
	if err := os.Remove(filepath.Join(w.stateDir(), e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
