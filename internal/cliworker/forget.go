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
// person's logins in it. So Reconcile also removes every directory whose recorded owner isn't the
// live account of its number (owner.go).

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
// refuses everything that would start for them from now on (ErrForgotten), then removes all of it
// the way removeUser does: their agent browsers, turns (the ones waiting for a place too),
// sign-ins, helper requests and processes stopped and waited for, so nothing writes into their
// directory any more, and only then the directory, with their logins, sessions, working
// directories and browser profile. Idempotent: a call that failed halfway (ErrBusy, say) is made
// again, and the mark stays.
//
// When ctx carries the account the call is meant for (internal/cliowner, a purge's), nothing is
// done once the number's directory is recorded as another account's: a restore gave the number to
// someone since, and the deleted account's files went with that account's introduction.
func (w *Worker) Forget(ctx context.Context, user int64) error {
	if user <= 0 {
		return errors.New("invalid user")
	}
	// The gate first: a disconnect, an introduction or another removal of the user's that is
	// running ends before this begins, and none begins meanwhile.
	end, err := w.beginGate(ctx, scope{user: user, all: true})
	if err != nil {
		return err
	}
	defer end()
	// Under the gate, so no introduction comes in between: the check holds for what follows.
	if err := ctx.Err(); err != nil {
		return err
	}
	if mine, err := w.ownedBy(ctx, user); err != nil || !mine {
		return err
	}
	if err := w.markForgotten(user); err != nil {
		return fmt.Errorf("recording that the user was forgotten: %w", err)
	}
	return w.removeUser(ctx, user)
}

// ErrIncomplete is Reconcile's answer when it could not remove every directory it should have
// (a process that didn't end in time, say): the users of those are refused (ErrForgotten) until a
// later Reconcile removes them, so the server can go on, and tries again.
var ErrIncomplete = errors.New("some users' directories could not be removed yet; they are refused until they are")

// Census is what the server's database says when it reads it, for Reconcile.
type Census struct {
	// Live is the numbers of the users that exist (an account whose purge has begun is not one).
	Live []int64 `json:"live"`
	// Owners is each live user's identity (users.cli_identity).
	Owners map[int64]string `json:"owners"`
	// Next is the number the next user gets: no number from it up has a user yet.
	Next int64 `json:"next"`
	// Boot is the server's own: a random value it takes when it starts, and introduces users
	// with (Introduce).
	Boot string `json:"boot"`
	// AdoptUnowned says a directory with no owner recorded (from before the worker recorded
	// owners) is the live account's of its number: in local mode, where the directories are in the
	// server's data directory and its backups. Otherwise such a directory may be another history's
	// (a restore, by any version, gives numbers again), and goes.
	AdoptUnowned bool `json:"adopt_unowned"`
	// Before is when the server read its database, for the forgotten marks: a mark made after it
	// is a purge the census can't know of.
	Before time.Time `json:"before"`
}

func (c Census) check() error {
	if c.Next <= 0 || c.Before.IsZero() || !identityPattern.MatchString(c.Boot) {
		return errors.New("invalid census")
	}
	for _, user := range c.Live {
		if user <= 0 || user >= c.Next || !identityPattern.MatchString(c.Owners[user]) {
			return errors.New("invalid census")
		}
	}
	return nil
}

// Reconcile brings the worker in line with the server's database, which a restore of the server's
// data may have taken back in its history (the server calls it at every start, before it asks
// anything else for a user). First the directories detached earlier that couldn't be deleted then
// (owner.go) are deleted. Then each user's directory and owner record is kept only when
//
//   - its recorded owner is the live account of its number, by identity; or
//   - it has no owner recorded (from before the worker recorded owners), its number is live, and
//     the census says such a directory is its account's (Census.AdoptUnowned, local mode): it is
//     adopted for that account; or
//   - its number is next or above and this server introduced its owner after it read its database
//     (the record's boot is the census's): a person added since.
//
// Anything else is removed the way removeUser removes it: a number below next that isn't live is
// an account gone for good, forgotten as Forget does, and stays refused; any other is someone of
// another history, and the number is given to its account afresh. No clock is asked. A directory
// that can't be removed yet has its user refused (ErrForgotten) until a later Reconcile removes
// it, and the answer is ErrIncomplete.
//
// Then the marks: a mark is removed when its user is live (their account is back) or its number is
// next or above (it will be given again), unless the mark was made after the census was read: that
// one is a purge running now, which the census can't know of. The marks of numbers below next that
// aren't live stay: those accounts are gone for good. The agent browsers' own refusals go the same
// way.
func (w *Worker) Reconcile(ctx context.Context, c Census) error {
	if err := c.check(); err != nil {
		return badRequest{err}
	}
	isLive := make(map[int64]bool, len(c.Live))
	for _, id := range c.Live {
		isLive[id] = true
	}
	var errs []error
	incomplete := map[int64]bool{}
	refuse := func(user int64, err error) error {
		// Refused until a later Reconcile removes it. A mark that can't be made fails the whole
		// call, and the server doesn't go on.
		if merr := w.markForgotten(user); merr != nil {
			return errors.Join(append(errs, err, merr)...)
		}
		incomplete[user] = true
		errs = append(errs, fmt.Errorf("user %d: %w", user, err))
		return nil
	}
	left, err := w.sweepTrash()
	if err != nil {
		return err
	}
	for user, err := range left {
		if ferr := refuse(user, err); ferr != nil {
			return ferr
		}
	}
	users, err := w.usersWithFiles()
	if err != nil {
		return err
	}
	for _, user := range users {
		if incomplete[user] {
			continue
		}
		keep := func() (bool, error) { return w.keeps(user, c, isLive[user]) }
		var err error
		removed := false
		if !isLive[user] && user < c.Next {
			removed, err = true, w.Forget(ctx, user)
		} else {
			removed, err = w.clearUnless(ctx, user, keep)
		}
		if err == nil {
			if removed {
				slog.Info("cliworker: removed a user's directory that isn't a live account's", "user", user)
			}
			continue
		}
		if ctx.Err() != nil {
			return errors.Join(append(errs, err)...)
		}
		if ferr := refuse(user, err); ferr != nil {
			return ferr
		}
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

// keeps says whether user's directory and owner record stay (see Reconcile), adopting a directory
// with no owner recorded for the live account when that is allowed.
func (w *Worker) keeps(user int64, c Census, live bool) (bool, error) {
	rec, ok, err := w.owner(user)
	if err != nil {
		return false, nil //nolint:nilerr // a damaged record is no owner: the directory goes
	}
	switch {
	case ok && live:
		return rec.identity == c.Owners[user], nil
	case ok:
		return user >= c.Next && rec.boot == c.Boot, nil
	case live && c.AdoptUnowned && w.hasDir(user):
		return true, w.setOwner(user, ownerRecord{identity: c.Owners[user], boot: c.Boot})
	}
	return false, nil
}

// clearUnless removes user's whole directory (removeUser) unless keep says it stays, asked again
// under the gate: an introduction may have come in between. It reports whether it removed it.
func (w *Worker) clearUnless(ctx context.Context, user int64, keep func() (bool, error)) (bool, error) {
	if yes, err := keep(); yes || err != nil {
		return false, err
	}
	end, err := w.beginGate(ctx, scope{user: user, all: true})
	if err != nil {
		return false, err
	}
	defer end()
	if yes, err := keep(); yes || err != nil {
		return false, err
	}
	return true, w.removeUser(ctx, user)
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

// usersWithFiles lists the users the worker keeps a directory or an owner record of, in order.
func (w *Worker) usersWithFiles() ([]int64, error) {
	seen := map[int64]bool{}
	for dir, prefix := range map[string]string{w.cfg.Layout.Root: "u", w.stateDir(): "owner-u"} {
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
