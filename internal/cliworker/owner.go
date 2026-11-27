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
	"time"
)

// Removing an account's files and processes (removeAccount) is tidying up: an account's home and
// uid are its own and never another's (accounts.go), so nothing of it can reach a later account,
// removed or not. It is still done carefully, so that nothing is left running or written after it.

// removeAccount removes what the worker keeps for acct, once the caller holds a gate that covers
// all its CLI accounts (nothing new starts for it). In order:
//
//  1. its agent browsers are stopped and waited for;
//  2. what runs for it (turns, sign-ins, helpers, the process groups those started) is stopped
//     and waited for (quiesce), and with isolation every process of its Unix user, one a CLI
//     detached too, for a bounded time;
//  3. its home is taken away from its path at once (detach), into the worker's own state
//     directory, and deleted there as the worker (removeTree);
//  4. what the worker remembers of it (sign-ins, finished turns with their permission questions)
//     goes.
//
// On an error nothing of a later step is done, and it can be tried again.
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
	// Made again, and checked again, on the account's next use: the home is about to leave its path,
	// and a detach that stops part way leaves no home there.
	w.dropUnixUser(acct.home)
	trash, err := w.detach(w.cfg.Layout.Home(acct.home).Root, "h"+strconv.FormatInt(acct.home, 10))
	if err != nil {
		return fmt.Errorf("taking the account's directory away: %w", err)
	}
	w.dropState(acct.id)
	if trash != "" {
		if err := removeTree(trash, w.cfg.Isolation != nil); err != nil {
			return fmt.Errorf("removing the account's old directory: %w", err)
		}
	}
	return nil
}

// uidWait is how long a removal waits for the processes of an account's Unix user to end.
const uidWait = 5 * time.Second

// dropState lets go of what the worker remembers of the account with identity id: its sign-ins and
// its turns, which quiesce has ended, with the permission questions they held.
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

// trashDir is where detached directories wait to be deleted: in the worker's own state
// directory, which no user can enter.
func (w *Worker) trashDir() string { return filepath.Join(w.stateDir(), "trash") }

// detach takes the directory root away from its path into the trash, under a name starting with
// name, and returns where it went ("" when there was none). No process of the account's runs by
// then. First it is renamed beside itself, out of its published path (a rename in the same
// directory doesn't write its ".."): only then, with isolation, does the worker take it over
// (CAP_CHOWN), since moving a directory to another parent writes its "..", and move it to the
// trash. A stop at any point leaves the published path either the account's own directory,
// untouched, or empty, never a directory of the wrong owner there; what is left beside it
// (detachedPrefix) is removed when a worker starts.
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
			return "", err // as a stop would: what is aside stays
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

// detachedPrefix names a directory a removal took off its published path, before it reached the
// trash.
const detachedPrefix = ".detached-"

// sweepDetached removes what a removal left beside the homes when it stopped (detach).
func (w *Worker) sweepDetached() error {
	left, _ := filepath.Glob(filepath.Join(w.cfg.Layout.Root, detachedPrefix+"*"))
	for _, p := range left {
		if err := removeTree(p, w.cfg.Isolation != nil); err != nil {
			return fmt.Errorf("cliworker: removing what a removal left: %w", err)
		}
	}
	return nil
}

// takeOver makes directory path the worker's own (with chown, when the users are Unix users of
// their own) and open to it alone. The caller checked that path is a directory in a directory
// only the worker writes, so nothing can have put a link in its place.
func takeOver(path string, chown bool) error {
	if chown {
		if err := os.Lchown(path, os.Getuid(), os.Getgid()); err != nil {
			return err
		}
	}
	return os.Chmod(path, 0o700) //nolint:gosec // G302: a directory needs x; 0700 is owner-only
}

// removeTree deletes path and everything under it, as the worker: each directory is taken over
// before it is read, so a read-only one (an archive's, a tool's) is no obstacle, and a link is
// removed, never followed. path is in a directory only the worker writes, and so, as it goes, is
// everything it looks at.
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

// sweepTrash deletes the directories detached earlier that couldn't be deleted then.
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

// removeLegacy removes a directory from before homes (Root/u<user>), detached first.
func (w *Worker) removeLegacy(user int64) error {
	name := "u" + strconv.FormatInt(user, 10)
	trash, err := w.detach(w.cfg.Layout.legacy(user), name)
	if err != nil || trash == "" {
		return err
	}
	return removeTree(trash, w.cfg.Isolation != nil)
}

// legacyDirs lists the numbers of the directories from before homes (Root/u<n>).
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
