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

var ErrNotReady = errors.New("the CLI worker isn't ready yet: try again in a moment")

func newBoot() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (s *Service) Installation(ctx context.Context) (string, error) {
	s.mu.Lock()
	inst := s.installation
	s.mu.Unlock()
	if inst != "" || s.db == nil {
		return inst, nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT installation FROM cli_installation WHERE id = 1`).Scan(&inst); err != nil {
		return "", fmt.Errorf("reading this server's installation: %w", err)
	}
	s.mu.Lock()
	s.installation = inst
	s.mu.Unlock()
	return inst, nil
}

func (s *Service) Account(ctx context.Context, user int64) (cliowner.Account, error) {
	inst, err := s.Installation(ctx)
	if err != nil {
		return cliowner.Account{}, err
	}
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
		s.mu.Lock()
		s.identities[user] = id
		s.mu.Unlock()
	}
	return cliowner.Account{User: user, Identity: id, Boot: s.boot, Installation: inst}, nil
}

type reconcileAttempt struct {
	done chan struct{}
	err  error
}

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

type userWorker interface {
	Available(cli cliworker.CLI) bool
	StartLogin(user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, error)
	LoginInfo(user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, bool, error)
	SubmitLoginCode(user int64, cli cliworker.CLI, slot cliworker.Slot, code string) error
	CancelLogin(user int64, cli cliworker.CLI, slot cliworker.Slot) error
	Status(ctx context.Context, user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.AuthStatus, error)
	Logout(ctx context.Context, user int64, cli cliworker.CLI, slot cliworker.Slot) error
	Forget(ctx context.Context, user int64) error
	Reconcile(ctx context.Context, census cliworker.Census) error
	Authority(ctx context.Context) (cliworker.Authority, error)
	StartTurn(spec cliworker.TurnSpec) (Turn, bool, error)
	FindTurn(user int64, id string) (Turn, bool, error)
	StopTurn(user int64, id string) error
	Permissions(ctx context.Context, user, conversation int64) ([]cliworker.PermissionRequest, error)
	Answer(ctx context.Context, user, conversation int64, id string, allow bool) error
	availability(cli cliworker.CLI) (bool, error)
}

type accountWorker struct {
	w Worker
	s *Service
}

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

func (a accountWorker) Forget(ctx context.Context, user int64) error {
	inst, err := a.s.Installation(ctx)
	if err != nil {
		return err
	}
	acct := cliowner.Account{User: user, Boot: a.s.boot, Installation: inst}
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

func (a accountWorker) Authority(ctx context.Context) (cliworker.Authority, error) {
	return a.w.Authority(ctx)
}

func (a accountWorker) StartTurn(spec cliworker.TurnSpec) (Turn, bool, error) {
	acct, err := a.quickAccount(spec.User)
	if err != nil {
		return nil, false, err
	}
	spec.Identity, spec.Boot, spec.Installation = acct.Identity, acct.Boot, acct.Installation
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

func (s *Service) cliAvailable(cli cliworker.CLI) (bool, error) {
	ok, err := s.worker.availability(cli)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrNotReady, err)
	}
	return ok, nil
}

func outage(err error) bool {
	return errors.Is(err, ErrNotReady) || errors.Is(err, cliworker.ErrUnreachable)
}

func (s *Service) Boot() string { return s.boot }

func (s *Service) LocalHome(a cliowner.Account) (string, error) {
	if aw, ok := s.worker.(accountWorker); ok {
		if l, ok := aw.w.(localWorker); ok {
			d, err := l.AccountDirs(a, "", "")
			return d.Root, err
		}
	}
	return "", errors.New("no CLI worker runs in this process")
}

func (s *Service) LocalHomeFound(a cliowner.Account) (string, bool, error) {
	if aw, ok := s.worker.(accountWorker); ok {
		if l, ok := aw.w.(localWorker); ok {
			dir, found := l.HomeDir(a.Identity)
			return dir, found, nil
		}
	}
	return "", false, errors.New("no CLI worker runs in this process")
}
