package cliaccounts

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"taskto.ai/taskto/internal/cliworker"
)

// A restore of the server's data goes back in its history, while the CLI worker's own directory
// (a volume of its own in container mode) doesn't: it may still hold the directory of someone who
// had a number in the lost history, with their logins, and the number is given again. So nothing
// is asked of the worker for a user, in this server's life, before
//
//   - the worker has been told who exists (ReconcileForgotten), which removes every directory whose
//     recorded owner isn't the live account of its number; and
//   - the user has been introduced to it (their identity, users.cli_identity), which removes a
//     directory that isn't theirs before anything of theirs is made, and records them as its owner.
//
// That is every user-scoped call of the worker (readyWorker) and the agent browser's (ReadyUser).
// The first such request tells the worker, if the start couldn't yet.

// ErrNotReady is the answer for a user while the CLI worker hasn't been told which users exist
// since this server started, and can't be now (it isn't reachable, say).
var ErrNotReady = errors.New("the CLI worker isn't ready yet: try again in a moment")

// newBoot is a server's own random value, which it introduces users with.
func newBoot() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand doesn't fail
	}
	return hex.EncodeToString(b[:])
}

// reconcileAttempt is one attempt at telling the worker who exists, which everyone who needs it
// waits for: done is closed when it has ended, and err says how.
type reconcileAttempt struct {
	done chan struct{}
	err  error
}

// startAttempt returns the attempt that runs, or starts one, in the background with its own time
// limit: a request that gives up waiting doesn't end it for the others.
func (s *Service) startAttempt() *reconcileAttempt {
	s.attemptMu.Lock()
	defer s.attemptMu.Unlock()
	if s.attempt != nil {
		return s.attempt
	}
	a := &reconcileAttempt{done: make(chan struct{})}
	s.mu.Lock()
	select {
	case <-s.closing:
		s.mu.Unlock()
		a.err = cliworker.ErrClosed
		close(a.done)
		return a
	default:
	}
	s.wg.Add(1)
	s.mu.Unlock()
	s.attempt = a
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
		defer cancel()
		a.err = s.ReconcileForgotten(ctx)
		s.attemptMu.Lock()
		s.attempt = nil
		s.attemptMu.Unlock()
		close(a.done)
	}()
	return a
}

// Ready makes sure the worker has been told which users exist since this server started, joining
// or starting an attempt if not, and is ErrNotReady if it can't be, or ctx ends first.
func (s *Service) Ready(ctx context.Context) error {
	if s.worker == nil || s.reconciled.Load() {
		return nil
	}
	a := s.startAttempt()
	select {
	case <-a.done:
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrNotReady, ctx.Err())
	}
	if s.reconciled.Load() {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrNotReady, a.err)
}

// ReadyUser is Ready, and that user has been introduced to the worker since this server started
// (introducing them now if not). The agent browser in the worker waits for it too.
func (s *Service) ReadyUser(ctx context.Context, user int64) error {
	if s.worker == nil {
		return nil
	}
	if err := s.Ready(ctx); err != nil {
		return err
	}
	if s.isIntroduced(user) {
		return nil
	}
	var identity string
	err := s.db.QueryRowContext(ctx, `SELECT cli_identity FROM users WHERE id = ?`, user).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return cliworker.ErrForgotten // no such account: nothing runs for it
	}
	if err != nil {
		return err
	}
	if err := s.worker.Introduce(ctx, user, identity, s.boot); err != nil {
		if errors.Is(err, cliworker.ErrForgotten) || errors.Is(err, cliworker.ErrBusy) || errors.Is(err, cliworker.ErrClosed) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrNotReady, err)
	}
	s.mu.Lock()
	s.introduced[user] = true
	s.mu.Unlock()
	return nil
}

// isIntroduced reports, without waiting, whether the worker was told who exists and who user is.
func (s *Service) isIntroduced(user int64) bool {
	if !s.reconciled.Load() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.introduced[user]
}

func (s *Service) forgetIntroduced(user int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.introduced, user)
}

// readyWorker is the worker as the service uses it: every call that acts for a user, or reads what
// the worker holds for them, waits for ReadyUser (or, where it can't wait, answers as if there
// were nothing). The worker refuses a user it doesn't know (cliworker.ErrUnknownOwner): one it
// forgot since (a Reconcile removed a directory) is introduced again, once.
type readyWorker struct {
	Worker
	s *Service
}

// call runs f for user once they are ready, and again after introducing them if the worker didn't
// know them.
func (r readyWorker) call(ctx context.Context, user int64, f func() error) error {
	if err := r.s.ReadyUser(ctx, user); err != nil {
		return err
	}
	err := f()
	if errors.Is(err, cliworker.ErrUnknownOwner) {
		r.s.forgetIntroduced(user)
		if err := r.s.ReadyUser(ctx, user); err != nil {
			return err
		}
		err = f()
	}
	return err
}

func (r readyWorker) StartLogin(user int64, cli cliworker.CLI, slot cliworker.Slot) (info cliworker.LoginInfo, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	defer cancel()
	err = r.call(ctx, user, func() error {
		var err error
		info, err = r.Worker.StartLogin(user, cli, slot)
		return err
	})
	return info, err
}

func (r readyWorker) LoginInfo(user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, bool) {
	if !r.s.isIntroduced(user) {
		return cliworker.LoginInfo{}, false
	}
	return r.Worker.LoginInfo(user, cli, slot)
}

func (r readyWorker) SubmitLoginCode(user int64, cli cliworker.CLI, slot cliworker.Slot, code string) error {
	ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	defer cancel()
	return r.call(ctx, user, func() error { return r.Worker.SubmitLoginCode(user, cli, slot, code) })
}

func (r readyWorker) CancelLogin(user int64, cli cliworker.CLI, slot cliworker.Slot) {
	if r.s.isIntroduced(user) {
		r.Worker.CancelLogin(user, cli, slot)
	}
}

func (r readyWorker) Status(ctx context.Context, user int64, cli cliworker.CLI, slot cliworker.Slot) (st cliworker.AuthStatus, err error) {
	err = r.call(ctx, user, func() error {
		var err error
		st, err = r.Worker.Status(ctx, user, cli, slot)
		return err
	})
	return st, err
}

func (r readyWorker) Logout(ctx context.Context, user int64, cli cliworker.CLI, slot cliworker.Slot) error {
	return r.call(ctx, user, func() error { return r.Worker.Logout(ctx, user, cli, slot) })
}

func (r readyWorker) StartTurn(spec cliworker.TurnSpec) (t Turn, attached bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	defer cancel()
	err = r.call(ctx, spec.User, func() error {
		var err error
		t, attached, err = r.Worker.StartTurn(spec)
		return err
	})
	return t, attached, err
}

// FindTurn is user's turn id: none until the worker was told who exists and who user is (a turn
// the worker kept from before may be the number's old owner's), and none of another user's.
func (r readyWorker) FindTurn(user int64, id string) (Turn, bool) {
	if !r.s.isIntroduced(user) {
		return nil, false
	}
	return r.Worker.FindTurn(user, id)
}

// StopTurn stops user's turn id, once user is introduced.
func (r readyWorker) StopTurn(user int64, id string) {
	if r.s.isIntroduced(user) {
		r.Worker.StopTurn(user, id)
	}
}

func (r readyWorker) Permissions(ctx context.Context, user, conversation int64) (out []cliworker.PermissionRequest, err error) {
	err = r.call(ctx, user, func() error {
		var err error
		out, err = r.Worker.Permissions(ctx, user, conversation)
		return err
	})
	return out, err
}

func (r readyWorker) Answer(ctx context.Context, user, conversation int64, id string, allow bool) error {
	return r.call(ctx, user, func() error { return r.Worker.Answer(ctx, user, conversation, id, allow) })
}

// availability is the worker's own answer to whether cli can run, with cliworker.ErrUnreachable
// when it can't be asked (Remote.Availability); a worker in this process always answers.
type availability interface {
	Availability(cli cliworker.CLI) (bool, error)
}

// cliAvailable reports whether cli can run on the worker, and an error when the worker can't be
// asked: an outage is the worker not being ready, not a CLI that isn't installed.
func (s *Service) cliAvailable(cli cliworker.CLI) (bool, error) {
	w := s.worker
	if r, ok := w.(readyWorker); ok {
		w = r.Worker
	}
	if a, ok := w.(availability); ok {
		return a.Availability(cli)
	}
	return s.worker.Available(cli), nil
}
