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
// to end (their turns, queued ones included, their logins and the short helper requests in
// flight); and only then removes the directory.
//
// A mark outlives the worker's restarts, but not a restore of the server's data: user numbers
// are never reused within one database's history, and a restore goes back in it (the person
// whose account was purged may be back, and the numbers after the backup are given again), while
// the worker's directory, in container mode, is not part of the backup. So the server says at
// every start which numbers are live and which come next (Reconcile), and the marks of those go.

// forgetWait is how long Forget gives what runs for the user to end, beyond a stopped CLI's own
// grace: the short helper requests are bounded to 30 s.
const forgetWait = 35 * time.Second

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

// beginQuick counts a short helper request for user that is about to run, or refuses it if the
// user was forgotten. Forget waits for the counted ones.
func (w *Worker) beginQuick(user int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.isForgotten(user) {
		return ErrForgotten
	}
	w.quick[user]++
	return nil
}

func (w *Worker) endQuick(user int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.quick[user]--; w.quick[user] <= 0 {
		delete(w.quick, user)
	}
	close(w.quickEnded)
	w.quickEnded = make(chan struct{})
}

// Forget removes everything the worker keeps for user, for an account that is deleted. It
// refuses everything that would start for them from now on (ErrForgotten), stops their turns
// (the ones waiting for a place too) and logins and waits for them and for the helper requests in
// flight to end, so nothing writes into their directory any more, and only then removes it,
// with their logins, sessions, working directories and browser profile. Idempotent: a call that failed halfway is
// made again, and the mark stays.
func (w *Worker) Forget(ctx context.Context, user int64) error {
	if user <= 0 {
		return errors.New("invalid user")
	}
	if err := w.markForgotten(user); err != nil {
		return fmt.Errorf("recording that the user was forgotten: %w", err)
	}
	// A turn or a login that started before the mark is in these maps (they are added under the
	// lock the check is made under); one that starts after it is refused.
	w.mu.Lock()
	var turns []*Turn
	for _, t := range w.turns {
		if t.spec.User == user {
			turns = append(turns, t)
		}
	}
	// Every execution, not only the current login of each account: a cancelled one that a new
	// sign-in replaced may still be running.
	var logins []*Login
	for l := range w.loginRuns {
		if l.user == user {
			logins = append(logins, l)
		}
	}
	w.mu.Unlock()
	for _, t := range turns {
		t.stop()
	}
	for _, l := range logins {
		l.stop()
	}

	wctx, cancel := context.WithTimeout(ctx, w.cfg.StopGrace+forgetWait)
	defer cancel()
	for _, t := range turns {
		select {
		case <-t.ended:
		case <-wctx.Done():
			return fmt.Errorf("a turn of the user didn't end: %w", wctx.Err())
		}
	}
	for _, l := range logins {
		select {
		case <-l.Done():
		case <-wctx.Done():
			return fmt.Errorf("a sign-in of the user didn't end: %w", wctx.Err())
		}
	}
	for {
		w.mu.Lock()
		n, ended := w.quick[user], w.quickEnded
		w.mu.Unlock()
		if n == 0 {
			break
		}
		select {
		case <-ended:
		case <-wctx.Done():
			return fmt.Errorf("a request for the user didn't end: %w", wctx.Err())
		}
	}

	_, _, err := w.runQuick(ctx, user, helperRequest{Op: "forget"})
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
	info, err := e.Info()
	if err != nil || !info.ModTime().Before(before) {
		return nil //nolint:nilerr // a mark that is gone or newer is left as it is
	}
	if err := os.Remove(filepath.Join(w.stateDir(), e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
