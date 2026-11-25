package main

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"taskto.ai/taskto/internal/drain"
)

// handlerWait is how long a shutdown waits for the handlers of a listener, after it has ended
// their contexts and closed their connections. A variable for tests.
var handlerWait = defaultHandlerWait

// The parts of a shutdown, each bounded. shutdownBudget is their sum: the time a stop takes at
// the very most, which the container's stop_grace_period (compose.yaml) must cover, or Docker
// kills the process before the drain has finished and the database is left to recover.
const (
	defaultShutdownGrace = 10 * time.Second // requests finish on their own, then are ended
	defaultHandlerWait   = 5 * time.Second  // then waited for, after their connections are closed
	emailWait            = 10 * time.Second // the emails the server accepted
	proxyGrace           = 5 * time.Second  // the CLI workers' proxy, which then waits as above too
	schedulerWait        = 10 * time.Second // a job's handler (schedule's default stop wait)
	sidecarStopWait      = 20 * time.Second // the agent sidecar: SIGTERM, 15 s, SIGKILL, its tools socket

	shutdownBudget = defaultShutdownGrace + defaultHandlerWait + emailWait +
		proxyGrace + defaultHandlerWait + schedulerWait + sidecarStopWait
)

// shutdownReport collects what could not be stopped cleanly while run shuts down. Whatever it is
// told was still running when it gave up (a handler that ignores its context) may use the
// database, so the database is then left open: closing it under that work would make its queries
// fail, or close its connection after run returned. The process is about to exit, and SQLite
// recovers its write-ahead log on the next start. Only run's own goroutine uses it, in its
// deferred stops.
type shutdownReport struct {
	errs []error
	busy bool
}

// note records what stopping the named part returned, if it failed.
func (r *shutdownReport) note(what string, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, drain.ErrBusy) {
		r.busy = true
	}
	slog.Error("shutdown: "+what+" did not stop cleanly", "err", err)
	r.errs = append(r.errs, fmt.Errorf("%s: %w", what, err))
}

func (r *shutdownReport) err() error { return errors.Join(r.errs...) }
