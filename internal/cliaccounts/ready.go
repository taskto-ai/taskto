package cliaccounts

import (
	"context"
	"errors"
	"fmt"

	"taskto.ai/taskto/internal/cliworker"
)

// A restore of the server's data goes back in its history, while the CLI worker's own directory
// (a volume of its own in container mode) doesn't: it may still hold the directory of someone who
// had a number in the lost history, with their logins, and the number is given again. The worker
// removes those once the server has told it who exists (ReconcileForgotten), so nothing is asked
// of it for a user before that, in this server's life: no sign-in, status check, sign-out or turn,
// and no agent browser (Ready). The first such request tells it, if the start couldn't yet.

// ErrNotReady is the answer for a user while the CLI worker hasn't been told which users exist
// since this server started, and can't be now (it isn't reachable, say).
var ErrNotReady = errors.New("the CLI worker isn't ready yet: try again in a moment")

// Ready makes sure the worker has been told which users exist since this server started, telling
// it now if not, and is ErrNotReady if it can't be. The agent browsers in the worker wait for it.
func (s *Service) Ready(ctx context.Context) error {
	if s.worker == nil || s.reconciled.Load() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	if s.reconciled.Load() {
		return nil
	}
	if err := s.reconcileLocked(ctx); err != nil && !s.reconciled.Load() {
		return fmt.Errorf("%w: %w", ErrNotReady, err)
	}
	return nil
}

// readyWorker is the worker as the service uses it: what acts for a user, and may make or read
// their directory, waits for Ready.
type readyWorker struct {
	Worker
	s *Service
}

func (r readyWorker) StartLogin(user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, error) {
	if err := r.s.Ready(context.Background()); err != nil {
		return cliworker.LoginInfo{}, err
	}
	return r.Worker.StartLogin(user, cli, slot)
}

func (r readyWorker) Status(ctx context.Context, user int64, cli cliworker.CLI, slot cliworker.Slot) (cliworker.AuthStatus, error) {
	if err := r.s.Ready(ctx); err != nil {
		return cliworker.AuthStatus{}, err
	}
	return r.Worker.Status(ctx, user, cli, slot)
}

func (r readyWorker) Logout(ctx context.Context, user int64, cli cliworker.CLI, slot cliworker.Slot) error {
	if err := r.s.Ready(ctx); err != nil {
		return err
	}
	return r.Worker.Logout(ctx, user, cli, slot)
}

func (r readyWorker) StartTurn(spec cliworker.TurnSpec) (Turn, bool, error) {
	if err := r.s.Ready(context.Background()); err != nil {
		return nil, false, err
	}
	return r.Worker.StartTurn(spec)
}
