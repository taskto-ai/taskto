package cliaccounts

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/cliworker"
)

// Every call to the CLI worker names the taskto account it is for by its number, its identity
// (users.cli_identity, an account's own for good and never another's) and this server's boot
// (cliowner.Account). The worker keeps everything of an account under its identity, in a home and,
// in container mode, a Unix user that no other account is ever given: so a restore of the
// server's data, which gives numbers again, never brings one account's files to another, whatever
// the worker kept, and nothing waits for the server to tell the worker who exists
// (ReconcileForgotten, which only tidies up).

// ErrNotReady is the answer while the CLI worker can't be reached (an outage, or a worker that
// hasn't started yet): try again in a moment.
var ErrNotReady = errors.New("the CLI worker isn't ready yet: try again in a moment")

// newBoot is a server's own random value, which every request to the worker carries.
func newBoot() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand doesn't fail
	}
	return hex.EncodeToString(b[:])
}

// Account is user's account as the worker knows it: their number, their identity and this
// server's boot. A user with no account (deleted) is cliworker.ErrForgotten.
func (s *Service) Account(ctx context.Context, user int64) (cliowner.Account, error) {
	s.mu.Lock()
	id, ok := s.identities[user]
	s.mu.Unlock()
	if !ok {
		err := s.db.QueryRowContext(ctx, `SELECT cli_identity FROM users WHERE id = ?`, user).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return cliowner.Account{}, cliworker.ErrForgotten
		}
		if err != nil {
			return cliowner.Account{}, err
		}
		// An account's identity never changes: it is kept for this server's life.
		s.mu.Lock()
		s.identities[user] = id
		s.mu.Unlock()
	}
	return cliowner.Account{User: user, Identity: id, Boot: s.boot}, nil
}

// reconcileAttempt is one attempt at telling the worker who exists, which StartReconcile waits for:
// done is closed when it has ended, and err says how.
type reconcileAttempt struct {
	done chan struct{}
	err  error
}

// startAttempt returns the attempt that runs, or starts one, in the background with its own time
// limit.
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

// userWorker is the worker as the service calls it, by user number: accountWorker names each
// call's account.
type userWorker interface {
	Available(cli cliworker.CLI) bool
	StartLogin(user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, error)
	// LoginInfo is user's sign-in for cli in slot; false when the worker has none, and the error
	// when it can't be asked.
	LoginInfo(user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, bool, error)
	SubmitLoginCode(user int64, cli cliworker.CLI, slot cliworker.Slot, code string) error
	CancelLogin(user int64, cli cliworker.CLI, slot cliworker.Slot) error
	Status(ctx context.Context, user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.AuthStatus, error)
	Logout(ctx context.Context, user int64, cli cliworker.CLI, slot cliworker.Slot) error
	Forget(ctx context.Context, user int64) error
	Reconcile(ctx context.Context, census cliworker.Census) error
	AuthorityLost(ctx context.Context) (string, error)
	StartTurn(spec cliworker.TurnSpec) (Turn, bool, error)
	// FindTurn is user's turn id; false when the worker has none, and the error when it can't be
	// asked.
	FindTurn(user int64, id string) (Turn, bool, error)
	StopTurn(user int64, id string) error
	Permissions(ctx context.Context, user, conversation int64) ([]cliworker.PermissionRequest, error)
	Answer(ctx context.Context, user, conversation int64, id string, allow bool) error
	// availability is Available with the outage told apart from a CLI that isn't installed.
	availability(cli cliworker.CLI) (bool, error)
}

// accountWorker is the worker with each call's account named.
type accountWorker struct {
	w Worker
	s *Service
}

// quickAccount is user's account for a call that brings no context of its own.
func (a accountWorker) quickAccount(user int64) (cliowner.Account, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return a.s.Account(ctx, user)
}

func (a accountWorker) Available(cli cliworker.CLI) bool { return a.w.Available(cli) }

func (a accountWorker) availability(cli cliworker.CLI) (bool, error) {
	if av, ok := a.w.(interface {
		Availability(cliworker.CLI) (bool, error)
	}); ok {
		return av.Availability(cli)
	}
	return a.w.Available(cli), nil
}

func (a accountWorker) StartLogin(user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, error) {
	acct, err := a.quickAccount(user)
	if err != nil {
		return cliworker.LoginInfo{}, err
	}
	return a.w.StartLogin(acct, cli, slot)
}

func (a accountWorker) LoginInfo(user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, bool, error) {
	acct, err := a.quickAccount(user)
	if err != nil {
		return cliworker.LoginInfo{}, false, err
	}
	info, err := a.w.LoginInfo(acct, cli, slot)
	if errors.Is(err, cliworker.ErrNoLogin) || errors.Is(err, cliworker.ErrUnknownAccount) {
		return cliworker.LoginInfo{}, false, nil
	}
	return info, err == nil, err
}

func (a accountWorker) SubmitLoginCode(user int64, cli cliworker.CLI, slot cliworker.Slot, code string) error {
	acct, err := a.quickAccount(user)
	if err != nil {
		return err
	}
	return a.w.SubmitLoginCode(acct, cli, slot, code)
}

func (a accountWorker) CancelLogin(user int64, cli cliworker.CLI, slot cliworker.Slot) error {
	acct, err := a.quickAccount(user)
	if err != nil {
		return err
	}
	return a.w.CancelLogin(acct, cli, slot)
}

func (a accountWorker) Status(ctx context.Context, user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.AuthStatus, error) {
	acct, err := a.s.Account(ctx, user)
	if err != nil {
		return cliworker.AuthStatus{}, err
	}
	return a.w.Status(ctx, acct, cli, slot)
}

func (a accountWorker) Logout(ctx context.Context, user int64, cli cliworker.CLI, slot cliworker.Slot) error {
	acct, err := a.s.Account(ctx, user)
	if err != nil {
		return err
	}
	return a.w.Logout(ctx, acct, cli, slot)
}

// Forget is for a deleted account: the identity is the one ctx carries (a purge's, kept with its
// steps, internal/cliowner), or else the account's if it is still there. With neither (a purge
// recorded before identities were kept, of an account gone since), it names none: the worker then
// removes only the number's directory from before identities.
func (a accountWorker) Forget(ctx context.Context, user int64) error {
	acct := cliowner.Account{User: user, Boot: a.s.boot}
	if id, ok := cliowner.From(ctx); ok {
		acct.Identity = id
	} else if found, err := a.s.Account(ctx, user); err == nil {
		acct = found
	} else if !errors.Is(err, cliworker.ErrForgotten) {
		return err
	}
	return a.w.Forget(ctx, acct)
}

func (a accountWorker) Reconcile(ctx context.Context, census cliworker.Census) error {
	return a.w.Reconcile(ctx, census)
}

func (a accountWorker) AuthorityLost(ctx context.Context) (string, error) {
	return a.w.AuthorityLost(ctx)
}

func (a accountWorker) StartTurn(spec cliworker.TurnSpec) (Turn, bool, error) {
	acct, err := a.quickAccount(spec.User)
	if err != nil {
		return nil, false, err
	}
	spec.Identity, spec.Boot = acct.Identity, acct.Boot
	return a.w.StartTurn(spec)
}

func (a accountWorker) FindTurn(user int64, id string) (Turn, bool, error) {
	acct, err := a.quickAccount(user)
	if err != nil {
		return nil, false, err
	}
	t, err := a.w.FindTurn(acct, id)
	if errors.Is(err, cliworker.ErrNotFound) {
		return nil, false, nil
	}
	return t, err == nil, err
}

func (a accountWorker) StopTurn(user int64, id string) error {
	acct, err := a.quickAccount(user)
	if err != nil {
		return err
	}
	return a.w.StopTurn(acct, id)
}

func (a accountWorker) Permissions(ctx context.Context, user, conversation int64) ([]cliworker.PermissionRequest, error) {
	acct, err := a.s.Account(ctx, user)
	if err != nil {
		return nil, err
	}
	return a.w.Permissions(ctx, acct, conversation)
}

func (a accountWorker) Answer(ctx context.Context, user, conversation int64, id string, allow bool) error {
	acct, err := a.s.Account(ctx, user)
	if err != nil {
		return err
	}
	return a.w.Answer(ctx, acct, conversation, id, allow)
}

// cliAvailable reports whether cli can run on the worker, and an error when the worker can't be
// asked: an outage is the worker not being ready, not a CLI that isn't installed.
func (s *Service) cliAvailable(cli cliworker.CLI) (bool, error) {
	ok, err := s.worker.availability(cli)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrNotReady, err)
	}
	return ok, nil
}

// outage reports whether err is the worker not being reachable.
func outage(err error) bool {
	return errors.Is(err, ErrNotReady) || errors.Is(err, cliworker.ErrUnreachable)
}

// Boot is this server's boot, which every request to the CLI worker carries: a deletion from it
// is the latest server's (cliworker.Worker.FenceDeletion).
func (s *Service) Boot() string { return s.boot }

// LocalHome is a's home directory on the CLI worker running in this process (local mode), for the
// browsers there; an error with no worker here.
func (s *Service) LocalHome(a cliowner.Account) (string, error) {
	if aw, ok := s.worker.(accountWorker); ok {
		if l, ok := aw.w.(localWorker); ok {
			d, err := l.AccountDirs(a, "", "")
			return d.Root, err
		}
	}
	return "", errors.New("no CLI worker runs in this process")
}

// LocalHomeFound is a's home directory on the CLI worker running in this process, retired or not,
// never made: false when the worker never gave it one. A purge removes the browser's files there.
func (s *Service) LocalHomeFound(a cliowner.Account) (string, bool, error) {
	if aw, ok := s.worker.(accountWorker); ok {
		if l, ok := aw.w.(localWorker); ok {
			dir, found := l.HomeDir(a.Identity)
			return dir, found, nil
		}
	}
	return "", false, errors.New("no CLI worker runs in this process")
}
