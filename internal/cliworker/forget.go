package cliworker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
//
// For the same reason a directory can outlive the history it belongs to: the person who had a
// number after the backup may have left their directory here (an account never purged, or one
// whose removal ended busy), and a new person given that number must not get it, with another
// person's logins in it. So Reconcile also removes every directory that isn't a live user's, and
// a live user's that the worker made before their account existed: the worker records when it
// first acts for each user (birthMark), before anything makes their directory.

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
// them (their turns, the ones waiting for a place too, their sign-ins, the helper requests in
// flight and their agent browsers) and waits for it, so nothing writes into their directory any
// more (removal.go), and only then removes it, with their logins, sessions, working directories
// and browser profile. Idempotent: a call that failed halfway (ErrBusy, say) is made again, and
// the mark stays.
func (w *Worker) Forget(ctx context.Context, user int64) error {
	if user <= 0 {
		return errors.New("invalid user")
	}
	// The gate first: a disconnect or another Forget of the user's that is running ends before this
	// begins, and none begins meanwhile, so their helper requests never overlap the removal.
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
	return w.removeUser(ctx, user)
}

// removeUser removes user's whole directory, once the caller has made sure nothing new starts for
// them (a mark, or a gate that covers all their accounts) and stopped what ran (quiesce). Their
// agent browsers are stopped first, and waited for: the profile is in the directory, and a
// browser's processes aren't the turns' and helpers' that quiesce stops. With a Unix user per
// taskto user, every process of that Unix user is then ended, wherever it came from. Then the helper removes
// what is inside, as the user, and the worker the directory itself, which is its in the layout's
// root, so the next person given the number gets a new one; and the record of when it was made.
func (w *Worker) removeUser(ctx context.Context, user int64) error {
	if b := w.browserStopper(); b != nil {
		if err := b.StopAll(ctx, user); err != nil {
			return fmt.Errorf("stopping the user's browsers: %w", err)
		}
	}
	// In container mode, whatever of the user's is still running is ended now: the processes the
	// stops above could not list, such as one a CLI or a browser detached from its process group.
	// Nothing new starts for them, so what is left after this runs no more.
	if err := w.endUserProcesses(ctx, user); err != nil {
		return fmt.Errorf("ending the user's processes: %w", err)
	}
	root := w.cfg.Layout.User(user).Root
	if _, err := os.Lstat(root); err == nil {
		if _, _, err := w.runHelper(ctx, user, helperRequest{Op: "forget"}); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Remove(root); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the user's directory: %w", err)
	}
	// With isolation, the next helper for the number makes the directory again (ensureUser).
	w.dropUnixUser(user)
	if err := os.Remove(w.birthMark(user)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ErrIncomplete is Reconcile's answer when it could not remove every directory it should have
// (a process that didn't end in time, say): the users of those are refused (ErrForgotten) until a
// later Reconcile removes them, so the server can go on, and tries again.
var ErrIncomplete = errors.New("some users' directories could not be removed yet; they are refused until they are")

// Census is what the server's database says when the server starts, for Reconcile.
type Census struct {
	// Live is the numbers of the users that exist (an account whose purge has begun is not one).
	Live []int64 `json:"live"`
	// Created is when each live user's account was made. A user it doesn't name isn't checked
	// against it.
	Created map[int64]time.Time `json:"created,omitempty"`
	// Next is the number the next user gets: no number from it up has a user yet.
	Next int64 `json:"next"`
	// Before is when the server read its database: what happened after it, the census can't know.
	Before time.Time `json:"before"`
}

// Reconcile brings the worker in line with the server's database, which a restore of the
// server's data may have taken back in time (the server calls it at every start). First the
// directories: a user's whole directory is removed, the way Forget removes it (what runs for them
// stopped and waited for first, their browsers too), when
//
//   - the number is below next and not live: an account that is gone for good (also one whose
//     removal ended busy). It is forgotten as Forget does, and stays refused;
//   - the number is next or above: whoever had it is in a history the restore went back from, and
//     the number will be given again. Not a directory the worker made after the census was read:
//     that is a person added since;
//   - the number is live, but the worker made the directory before the account was made: it is
//     the directory of someone who had the number in a lost history. A directory from before the
//     worker recorded when it made them is judged by its own time, which only ever comes later.
//
// Nothing of a live user is removed otherwise. A directory that can't be removed yet (something of
// it didn't stop in time) has its user refused (ErrForgotten) until a later Reconcile removes it,
// and the answer is ErrIncomplete.
//
// Then the marks: a mark is removed when its user is live (their account is back) or its number is
// next or above (it will be given again), unless the mark was made after the census was read: that
// one is a purge running now, which the census can't know of. The marks of numbers below next that
// aren't live stay: those accounts are gone for good. The agent browsers' own refusals go the same
// way.
func (w *Worker) Reconcile(ctx context.Context, c Census) error {
	if c.Next <= 0 || c.Before.IsZero() {
		return errors.New("invalid census")
	}
	isLive := make(map[int64]bool, len(c.Live))
	for _, id := range c.Live {
		isLive[id] = true
	}
	users, err := w.usersWithFiles()
	if err != nil {
		return err
	}
	var errs []error
	incomplete := map[int64]bool{}
	for _, user := range users {
		var err error
		removed := false
		why := ""
		switch created, checked := c.Created[user]; {
		case !isLive[user] && user < c.Next:
			why, err = "an account that is gone", w.Forget(ctx, user)
			removed = err == nil
		case !isLive[user]:
			why = "a number the server gives again"
			removed, err = w.clearStale(ctx, user, func(born time.Time, recorded bool) bool {
				return !recorded || born.Before(c.Before)
			})
		case checked:
			why = "made before the account"
			removed, err = w.clearStale(ctx, user, func(born time.Time, _ bool) bool { return born.Before(created) })
		}
		if err == nil {
			if removed {
				slog.Info("cliworker: removed a user's directory the server's database has no owner of", "user", user, "why", why)
			}
			continue
		}
		if ctx.Err() != nil {
			return errors.Join(append(errs, err)...)
		}
		// Refused until a later Reconcile removes it. A mark that can't be made fails the whole
		// call, and the server doesn't go on.
		if merr := w.markForgotten(user); merr != nil {
			return errors.Join(append(errs, err, merr)...)
		}
		incomplete[user] = true
		errs = append(errs, fmt.Errorf("user %d: %w", user, err))
	}

	if err := w.takeBackMarks(c, isLive, incomplete); err != nil {
		return errors.Join(append(errs, err)...)
	}
	if b := w.browserStopper(); b != nil {
		b.Unforget(func(user int64) bool { return (isLive[user] || user >= c.Next) && !incomplete[user] }, c.Before)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrIncomplete, errors.Join(errs...))
	}
	return nil
}

// takeBackMarks removes the marks Reconcile takes back (see there), apart from those of the users
// in keep.
func (w *Worker) takeBackMarks(c Census, isLive, keep map[int64]bool) error {
	entries, err := os.ReadDir(w.stateDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		id, ok := strings.CutPrefix(e.Name(), "forgotten-u")
		if !ok {
			continue
		}
		user, err := strconv.ParseInt(id, 10, 64)
		if err != nil || user <= 0 || (!isLive[user] && user < c.Next) || keep[user] {
			continue
		}
		if err := w.removeOldMark(e, c.Before); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// clearStale removes user's whole directory if stale says it isn't theirs, given when the worker
// first acted for them (born, when recorded). It is asked again once nothing can start for the
// user and what ran was stopped: a person added after the census may have been given the number
// meanwhile, and their directory is theirs.
func (w *Worker) clearStale(ctx context.Context, user int64, stale func(born time.Time, recorded bool) bool) (removed bool, err error) {
	if yes, err := w.stale(user, stale); !yes || err != nil {
		return false, err
	}
	sc := scope{user: user, all: true}
	end, err := w.beginGate(ctx, sc)
	if err != nil {
		return false, err
	}
	defer end()
	if yes, err := w.stale(user, stale); !yes || err != nil {
		return false, err
	}
	if err := w.quiesce(ctx, sc); err != nil {
		return false, err
	}
	if yes, err := w.stale(user, stale); !yes || err != nil {
		return false, err
	}
	return true, w.removeUser(ctx, user)
}

// stale asks stale about user's directory: when the worker recorded first acting for them, or,
// for a directory from before it recorded that, the directory's own time (when it was made or
// last changed, so never before it was made). A user with neither has nothing to remove. A
// directory that can't be looked at is an error: nothing is removed, and the user is refused.
func (w *Worker) stale(user int64, stale func(born time.Time, recorded bool) bool) (bool, error) {
	if born, ok := w.born(user); ok {
		return stale(born, true), nil
	}
	info, err := os.Lstat(w.cfg.Layout.User(user).Root)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// The file system's clock may be a little behind the one accounts are dated by: a directory
	// counts as older than a time only by more than that.
	return stale(info.ModTime().Add(fsClockSlack), false), nil
}

// fsClockSlack is more than a file's time can be behind the clock that dates accounts and the
// census (file systems that take it from a coarse clock lag by a few milliseconds).
const fsClockSlack = time.Second

// usersWithFiles lists the users the worker keeps a directory of, or a record of when it made
// one, in order.
func (w *Worker) usersWithFiles() ([]int64, error) {
	seen := map[int64]bool{}
	for dir, prefix := range map[string]string{w.cfg.Layout.Root: "u", w.stateDir(): "born-u"} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			id, ok := strings.CutPrefix(e.Name(), prefix)
			if !ok {
				continue
			}
			// Only a name the layout makes: u7, never u07 or u+7.
			if user, err := strconv.ParseInt(id, 10, 64); err == nil && user > 0 && strconv.FormatInt(user, 10) == id {
				seen[user] = true
			}
		}
	}
	users := make([]int64, 0, len(seen))
	for user := range seen {
		users = append(users, user)
	}
	slices.Sort(users)
	return users, nil
}

// birthMark is the file in the worker's own directory that records when the worker first acted for
// user, in its contents: before that, nothing of theirs was made here. Not the file's own time,
// which some file systems take from a clock a few milliseconds behind: a user's first use right
// after their account was made would look older than the account.
func (w *Worker) birthMark(user int64) string {
	return filepath.Join(w.stateDir(), "born-u"+strconv.FormatInt(user, 10))
}

// born is when the worker first acted for user, if it recorded that. A record whose time can't be
// read (a crash as it was written) is dated by the file's own time, with the file system clock's
// slack (fsClockSlack).
func (w *Worker) born(user int64) (time.Time, bool) {
	b, err := os.ReadFile(w.birthMark(user))
	if err != nil {
		return time.Time{}, false
	}
	if at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b))); err == nil {
		return at, true
	}
	info, err := os.Lstat(w.birthMark(user))
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime().Add(fsClockSlack), true
}

// noteBirth records, before the worker first acts for user, that it does now. It is the worker's
// own file: nothing of a user's can change it. A directory made before the worker recorded this
// gets the time of its first use since, which is after its account was made.
func (w *Worker) noteBirth(user int64) error {
	path := w.birthMark(user)
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(w.stateDir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: the worker's own state directory
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.WriteString(time.Now().UTC().Format(time.RFC3339Nano) + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	// On disk before the directory it dates: Reconcile takes a directory without one for older.
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
	return d.Sync()
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
