package cliaccounts

import (
	"context"
	"sync/atomic"

	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/cliworker"
)

// swapWorker is a Worker a test can change while the service runs (a worker that stops being
// reachable, one started again): the service keeps one reference, and each call reads the current
// worker atomically, so a sign-in's watcher or a turn reading it doesn't race the test.
type swapWorker struct{ cur atomic.Pointer[Worker] }

// swappable installs a swapWorker in e's service, with e's own worker in it, before the test
// starts anything that reads the service's worker.
func (e *env) swappable() *swapWorker {
	sw := &swapWorker{}
	sw.use(LocalWorker(e.worker))
	e.svc.worker = accountWorker{w: sw, s: e.svc}
	return sw
}

func (s *swapWorker) use(w Worker) { s.cur.Store(&w) }
func (s *swapWorker) w() Worker    { return *s.cur.Load() }

func (s *swapWorker) Available(cli cliworker.CLI) bool { return s.w().Available(cli) }

// Availability answers as the current worker does, which may say it can't be reached.
func (s *swapWorker) Availability(cli cliworker.CLI) (bool, error) {
	if av, ok := s.w().(interface {
		Availability(cliworker.CLI) (bool, error)
	}); ok {
		return av.Availability(cli)
	}
	return s.w().Available(cli), nil
}

func (s *swapWorker) StartLogin(a cliowner.Account, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, error) {
	return s.w().StartLogin(a, cli, slot)
}

func (s *swapWorker) LoginInfo(a cliowner.Account, cli cliworker.CLI, slot cliworker.Slot) (cliworker.LoginInfo, error) {
	return s.w().LoginInfo(a, cli, slot)
}

func (s *swapWorker) SubmitLoginCode(a cliowner.Account, cli cliworker.CLI, slot cliworker.Slot, code string) error {
	return s.w().SubmitLoginCode(a, cli, slot, code)
}

func (s *swapWorker) CancelLogin(a cliowner.Account, cli cliworker.CLI, slot cliworker.Slot) error {
	return s.w().CancelLogin(a, cli, slot)
}

func (s *swapWorker) Status(ctx context.Context, a cliowner.Account, cli cliworker.CLI, slot cliworker.Slot) (cliworker.AuthStatus, error) {
	return s.w().Status(ctx, a, cli, slot)
}

func (s *swapWorker) Logout(ctx context.Context, a cliowner.Account, cli cliworker.CLI, slot cliworker.Slot) error {
	return s.w().Logout(ctx, a, cli, slot)
}

func (s *swapWorker) Forget(ctx context.Context, a cliowner.Account) error {
	return s.w().Forget(ctx, a)
}

func (s *swapWorker) Reconcile(ctx context.Context, census cliworker.Census) error {
	return s.w().Reconcile(ctx, census)
}

func (s *swapWorker) AuthorityLost(ctx context.Context) (string, error) {
	return s.w().AuthorityLost(ctx)
}

func (s *swapWorker) StartTurn(spec cliworker.TurnSpec) (Turn, bool, error) {
	return s.w().StartTurn(spec)
}

func (s *swapWorker) FindTurn(a cliowner.Account, id string) (Turn, error) {
	return s.w().FindTurn(a, id)
}

func (s *swapWorker) StopTurn(a cliowner.Account, id string) error { return s.w().StopTurn(a, id) }

func (s *swapWorker) Permissions(ctx context.Context, a cliowner.Account, conversation int64) ([]cliworker.PermissionRequest, error) {
	return s.w().Permissions(ctx, a, conversation)
}

func (s *swapWorker) Answer(ctx context.Context, a cliowner.Account, conversation int64, id string, allow bool) error {
	return s.w().Answer(ctx, a, conversation, id, allow)
}
