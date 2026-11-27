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

var ErrForgotten = errors.New("this account's CLI files were removed with it")

var ErrIncomplete = errors.New("some retired accounts' files could not be removed yet; they are refused, and it is tried again")

func (w *Worker) Forget(ctx context.Context, a cliowner.Account) error {
	if a.User <= 0 || (a.Identity != "" && !cliowner.IsIdentity(a.Identity)) {
		return badRequest{errors.New("invalid account")}
	}
	w.censusMu.Lock()
	defer w.censusMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.deletionFence(a); err != nil {
		return err
	}
	if a.Identity == "" {
		return w.removeLegacy(a.User)
	}
	end, err := w.beginGate(ctx, scope{id: a.Identity, all: true})
	if err != nil {
		return err
	}
	defer end()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.retire(a.Identity, a.User); err != nil {
		return fmt.Errorf("recording that the account is gone: %w", err)
	}
	r, ok, err := w.find(a.Identity)
	if err != nil || !ok || r.Home == 0 {
		return err
	}
	if err := w.removeAccount(ctx, account{id: a.Identity, user: r.User, home: r.Home}); err != nil {
		return err
	}
	return w.markCleaned(a.Identity)
}

type Census struct {
	Owners       map[int64]string `json:"owners"`
	Boot         string           `json:"boot"`
	Installation string           `json:"installation"`
	Before       time.Time        `json:"before"`
}

func (c Census) check() error {
	if c.Before.IsZero() || !cliowner.IsIdentity(c.Boot) || !cliowner.IsIdentity(c.Installation) {
		return errors.New("invalid census")
	}
	for user, id := range c.Owners {
		if user <= 0 || !cliowner.IsIdentity(id) {
			return errors.New("invalid census")
		}
	}
	return nil
}

func (w *Worker) Reconcile(ctx context.Context, c Census) error {
	if err := c.check(); err != nil {
		return badRequest{err}
	}
	n := w.reserveBoot(c.Boot)
	defer w.unreserveBoot(c.Boot)
	if hook := w.arrivedHookForTest; hook != nil {
		hook(c.Boot)
	}
	w.censusMu.Lock()
	defer w.censusMu.Unlock()
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
		case r.Installation != "" && r.Installation != c.Installation:
		case !r.Retired && (r.Boot == c.Boot || r.Gen >= seq):
		default:
			if err := ctx.Err(); err != nil {
				return errors.Join(append(errs, err)...)
			}
			_, err := w.retireNow(id, r.User)
			if err != nil {
				return err
			}
			if hook := w.retiredHookForTest; hook != nil {
				hook(id)
			}
			if r.Home == 0 {
				continue
			}
			if err := w.tidy(context.WithoutCancel(ctx), account{id: id, user: r.User, home: r.Home}, r.Retired && r.Cleaned); err != nil {
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
		b.Unforget(func(id string) bool { return live[id] && !w.retired(id) }, time.Now())
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrIncomplete, errors.Join(errs...))
	}
	return nil
}

func (w *Worker) tidy(ctx context.Context, acct account, cleaned bool) error {
	if _, err := os.Lstat(w.cfg.Layout.Home(acct.home).Root); cleaned && errors.Is(err, fs.ErrNotExist) {
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
	return w.markCleaned(acct.id)
}

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
