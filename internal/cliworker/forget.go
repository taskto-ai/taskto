package cliworker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"taskto.ai/taskto/internal/cliowner"
)

// ErrForgotten: the account is retired (a deleted account, or one of a history a restore of the
// server's data went back from): nothing starts for it.
var ErrForgotten = errors.New("this account's CLI files were removed with it")

// ErrIncomplete is Reconcile's answer when it couldn't remove everything of the accounts it
// retired yet (a process that didn't end in time, say). They are refused anyway; the server tries
// again.
var ErrIncomplete = errors.New("some retired accounts' files could not be removed yet; they are refused, and it is tried again")

// Forget retires the account a names, for an account that is deleted, and removes what the worker
// keeps of it: its turns (the ones waiting for a place too), sign-ins, helper requests, browsers
// and processes are stopped and waited for, then its home goes, with its logins, sessions, working
// directories and browser profile. The retirement is durable and comes first: nothing starts for
// the account from then on, even if the removal fails (ErrBusy, say) and is tried again. Only that
// account's home is ever touched: another account given the number since has a home of its own.
//
// A purge step from before identities were kept names none: then only the number's directory from
// before homes (Root/u<n>), if any, goes.
func (w *Worker) Forget(ctx context.Context, a cliowner.Account) error {
	if a.User <= 0 {
		return badRequest{errors.New("invalid user")}
	}
	if a.Identity == "" {
		if err := ctx.Err(); err != nil {
			return err
		}
		return w.removeLegacy(a.User)
	}
	if !cliowner.IsIdentity(a.Identity) {
		return badRequest{errors.New("invalid account")}
	}
	// One at a time with the censuses, and only for the latest server boot: a deletion an earlier
	// server sent, delayed past a restore that brought the account back, retires nothing. One whose
	// caller has gone while it waited (its server stopped) does nothing.
	w.censusMu.Lock()
	defer w.censusMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.deletionFence(a.Boot); err != nil {
		return err
	}
	end, err := w.beginGate(ctx, scope{id: a.Identity, all: true})
	if err != nil {
		return err
	}
	defer end()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Retired first, and also when the worker has nothing of it: nothing is made for it later.
	if err := w.retire(a.Identity, a.User); err != nil {
		return fmt.Errorf("recording that the account is gone: %w", err)
	}
	r, ok, err := w.find(a.Identity)
	if err != nil || !ok || r.Home == 0 {
		return err // nothing of the account is here
	}
	return w.removeAccount(ctx, account{id: a.Identity, user: r.User, home: r.Home})
}

// Census is what the server's database says when it reads it, for Reconcile.
type Census struct {
	// Owners is each live user's identity (users.cli_identity), by number; an account whose purge
	// has begun is not live.
	Owners map[int64]string `json:"owners"`
	// Boot is the server's own: a random value it takes when it starts, and sends with every
	// request (cliowner.Account).
	Boot string `json:"boot"`
	// Before is when the server read its database, for the logs: nothing is decided by it (the
	// worker orders boots by when it registered them, volume.go).
	Before time.Time `json:"before"`
}

func (c Census) check() error {
	if c.Before.IsZero() || !cliowner.IsIdentity(c.Boot) {
		return errors.New("invalid census")
	}
	for user, id := range c.Owners {
		if user <= 0 || !cliowner.IsIdentity(id) {
			return errors.New("invalid census")
		}
	}
	return nil
}

// Reconcile brings the worker in line with the server's database, which a restore of the server's
// data may have taken back in its history (the server calls it at every start, and again until it
// succeeds). For each account the worker has a record of:
//
//   - live in the census: a retirement made before the census was registered is taken back (a
//     restore brought the account back);
//   - not live, but made by a request of this server's boot, or made since the census was
//     registered: kept;
//   - otherwise: retired, and its files and processes removed (tidying up: its home and uid are
//     never another account's).
//
// The directories from before homes (Root/u<n>) are removed too, unless the worker adopts them
// (Config.AdoptLegacy, local mode), and what an earlier removal left in the trash is deleted. The
// agent browsers' own refusals of live accounts are taken back. What couldn't be removed yet makes
// the answer ErrIncomplete; the accounts are refused all the same.
func (w *Worker) Reconcile(ctx context.Context, c Census) error {
	if err := c.check(); err != nil {
		return badRequest{err}
	}
	// The boot's number is given as the census arrives, before it waits for the lock: of two
	// censuses, the one that arrived first has the lower number, whichever runs first.
	n := w.reserveBoot(c.Boot)
	defer w.unreserveBoot(c.Boot)
	if hook := w.arrivedHookForTest; hook != nil {
		hook(c.Boot)
	}
	// One at a time, and only the latest server boot's: a census an earlier server sent (one a
	// restore replaced, still running or delayed) would retire the accounts the later one made.
	w.censusMu.Lock()
	defer w.censusMu.Unlock()
	// A census whose caller has gone while it waited (its server stopped: one server runs on a
	// data directory, so an earlier server's census is late only if it was in flight then) does
	// nothing, and registers nothing.
	if err := ctx.Err(); err != nil {
		return err
	}
	seq, err := w.registerCensus(c, n)
	if err != nil {
		return err
	}
	live := make(map[string]bool, len(c.Owners))
	for _, id := range c.Owners {
		live[id] = true
	}
	recs, err := w.records()
	if err != nil {
		return err
	}
	var errs []error
	for id, r := range recs {
		switch {
		case live[id]:
			if err := w.unretire(id, seq); err != nil {
				return err
			}
		case !r.Retired && (r.Boot == c.Boot || r.Gen >= seq):
			// Made by this census's server since it read its database, or made since the census was
			// registered (by a later server, whose census is still to come). Kept.
		default:
			// A caller that has given up retires nothing more.
			if err := ctx.Err(); err != nil {
				return errors.Join(append(errs, err)...)
			}
			already, err := w.retireNow(id, r.User)
			if err != nil {
				return err
			}
			if hook := w.retiredHookForTest; hook != nil {
				hook(id)
			}
			if r.Home == 0 {
				continue
			}
			// Retired and stopped are one step: once the retirement is recorded, what runs for the
			// account is stopped even if the caller has gone meanwhile. A retry that finds it
			// retired would otherwise not stop it (tidy).
			if err := w.tidy(context.WithoutCancel(ctx), account{id: id, user: r.User, home: r.Home}, already); err != nil {
				if ctx.Err() != nil {
					return errors.Join(append(errs, err)...)
				}
				errs = append(errs, fmt.Errorf("account %d: %w", r.User, err))
			}
		}
	}
	if !w.cfg.AdoptLegacy {
		legacy, err := w.legacyDirs()
		if err != nil {
			return err
		}
		for _, user := range legacy {
			if err := w.removeLegacy(user); err != nil {
				errs = append(errs, fmt.Errorf("the directory of number %d from before homes: %w", user, err))
			} else {
				slog.Info("cliworker: removed a directory from before homes", "user", user)
			}
		}
	}
	if err := w.sweepTrash(); err != nil {
		errs = append(errs, err)
	}
	if b := w.browserStopper(); b != nil {
		// The records decide, not a clock: the browsers' refusals of the accounts this census
		// brought back go, and those of accounts retired since it was registered (a deletion
		// running now) stay. Deletions wait for the census lock, so every refusal there is now
		// was made before this point of the worker's own time.
		b.Unforget(func(id string) bool { return live[id] && !w.retired(id) }, time.Now())
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrIncomplete, errors.Join(errs...))
	}
	return nil
}

// tidy removes a retired account's processes and home. An account retired just now may have work
// admitted before (a first turn, a sign-in, a browser) whose helper hasn't made its home yet: the
// gate is closed and that work stopped and waited for whether or not the home is there, so nothing
// makes it afterwards. One retired already, at an earlier census or deletion, had that done then,
// and nothing was admitted since: with no home left there is nothing to do.
func (w *Worker) tidy(ctx context.Context, acct account, already bool) error {
	if _, err := os.Lstat(w.cfg.Layout.Home(acct.home).Root); already && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	end, err := w.beginGate(ctx, scope{id: acct.id, all: true})
	if err != nil {
		return err
	}
	defer end()
	if err := w.removeAccount(ctx, acct); err != nil {
		return err
	}
	slog.Info("cliworker: removed a retired account's directory", "user", acct.user)
	return nil
}

// ForgetLocal removes what local mode's worker keeps of a deleted account under layout, when no
// local worker runs to do it (the CLI workers are off, or in container mode now): identity's
// home, found through its record, which is retired first so a local worker started later refuses
// it; and the number's directory from before homes. A record that isn't there means no home.
// The local worker's own Forget does the same and more (it stops what runs): when it runs, it
// is asked first, and this then finds nothing left.
func ForgetLocal(layout Layout, identity string, user int64) error {
	if identity != "" {
		if !cliowner.IsIdentity(identity) {
			return badRequest{errors.New("invalid account")}
		}
		w := &Worker{cfg: Config{Layout: layout}, accounts: map[string]accountRecord{}}
		r, ok, err := w.find(identity)
		if err != nil {
			return err
		}
		if ok {
			if err := w.retire(identity, r.User); err != nil {
				return err
			}
			if r.Home != 0 {
				if err := removeTree(layout.Home(r.Home).Root, false); err != nil {
					return err
				}
				if err := syncDir(layout.Root); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
		}
	}
	if user > 0 {
		return removeTree(layout.legacy(user), false)
	}
	return nil
}
