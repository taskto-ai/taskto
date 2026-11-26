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

// Forgetting a user (a deleted account) must not race what the worker runs for them: a CLI
// that goes on running writes its session and refreshed credentials into a directory that is
// being removed (and makes it again), and a turn, a sign-in or a status check that starts
// afterwards makes the directory and a login's files in it again. So Forget first marks the user
// forgotten, durably, which refuses everything that would start for them (Start, StartLogin
// and every helper request, helperCommand); then stops what is running for them and waits for it
// to end (removal.go); and only then removes the directory.
//
// A mark outlives the worker's restarts, but not a restore of the server's data: user numbers
// are never reused within one database's history, and a restore goes back in it (the person
// whose account was purged may be back, and the numbers after the backup are given again), while
// the worker's directory, in container mode, is not part of the backup. So the server says at
// every start which numbers are live and which come next (Reconcile), and the marks of those go.

// forgottenMark is the file in the worker's own directory that records user as forgotten.
func (w *Worker) forgottenMark(user int64) string {
	return filepath.Join(w.stateDir(), "forgotten-u"+strconv.FormatInt(user, 10))
}

// isForgotten reports whether user was forgotten (Forget): nothing runs for them any more. A
// mark that can't be read counts: only its absence says the user was not forgotten.
func (w *Worker) isForgotten(user int64) bool {
	_, err := os.Stat(w.forgottenMark(user))
	return !errors.Is(err, fs.ErrNotExist)
}

// markForgotten records user as forgotten, durably: a worker that restarts still refuses them.
func (w *Worker) markForgotten(user int64) error {
	// Not at the same moment as Reconcile looks at the mark's time and removes it.
	w.marks.Lock()
	defer w.marks.Unlock()
	if err := os.MkdirAll(w.stateDir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(w.forgottenMark(user), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// A mark made again is as new as this call (Reconcile leaves the ones made after its snapshot).
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
	// The mark's directory entry is what counts: it must be on disk before anything is removed.
	d, err := os.Open(w.stateDir())
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return err
	}
	// And the state directory's own entry, in the layout's root.
	root, err := os.Open(w.cfg.Layout.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Sync()
}

// Forget removes everything the worker keeps for user, for an account that is deleted. It
// refuses everything that would start for them from now on (ErrForgotten), stops what runs for
// them (their turns, the ones waiting for a place too, their sign-ins and the helper requests in
// flight) and waits for it, so nothing writes into their directory any more (removal.go), and only
// then removes it, with their logins, sessions, working directories and browser profile.
// Idempotent: a call that failed halfway (ErrBusy, say) is made again, and the mark stays.
func (w *Worker) Forget(ctx context.Context, user int64) error {
	if user <= 0 {
		return errors.New("invalid user")
	}
	if err := w.markForgotten(user); err != nil {
		return fmt.Errorf("recording that the user was forgotten: %w", err)
	}
	if err := w.quiesce(ctx, scope{user: user, all: true}); err != nil {
		return err
	}
	_, _, err := w.runHelper(ctx, user, helperRequest{Op: "forget"})
	return err
}

// Reconcile takes back the marks that a restore of the server's data made wrong. The server
// calls it at every start with what its database says: live holds the numbers of the users
// that exist, next the number the next user gets, and before is the time it read them. A mark
// is removed when its user is live (their account is back) or its number is next or above (it
// will be given again), unless the mark was made after before: that one is a purge running now,
// which the snapshot can't know of. The marks of numbers below next that aren't live stay: those
// accounts are gone for good.
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

// removeOldMark removes mark e unless it was made after before. The time is read and the mark
// removed under the lock that making a mark holds, so a mark made again in between stays.
func (w *Worker) removeOldMark(e fs.DirEntry, before time.Time) error {
	w.marks.Lock()
	defer w.marks.Unlock()
	// Read now, under the lock: a directory entry's own Info may be from before the mark was made again.
	info, err := os.Lstat(filepath.Join(w.stateDir(), e.Name()))
	if err != nil || !info.ModTime().Before(before) {
		return nil //nolint:nilerr // a mark that is gone or newer is left as it is
	}
	if err := os.Remove(filepath.Join(w.stateDir(), e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
