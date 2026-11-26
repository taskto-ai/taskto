package main

import (
	"context"
	"errors"
	"testing"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/cliaccounts"
	"taskto.ai/taskto/internal/cliworker"
)

// unreachableWorker is a CLI worker that can't be told who exists: the server can't reach it.
type unreachableWorker struct{ cliaccounts.Worker }

func (unreachableWorker) Reconcile(context.Context, cliworker.Census) error {
	return errors.New("the CLI worker is unreachable")
}

// Until the CLI worker has been told who exists since the server started, the agent browser,
// whose profile is in the user's CLI directory, is refused with the reason's own code, which the
// web app translates.
func TestBrowserWaitsForTheWorkerToBeTold(t *testing.T) {
	cli := cliaccounts.New(cliWorkersDB(t), cliaccounts.Container, unreachableWorker{})
	t.Cleanup(cli.Close)
	err := browserUsable(cli)(context.Background())
	var e *apierr.Error
	if !errors.As(err, &e) || e.Code != apierr.CodeCLIWorkerNotReady {
		t.Errorf("browser usable = %v, want %s", err, apierr.CodeCLIWorkerNotReady)
	}
}
