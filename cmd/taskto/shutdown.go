package main

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"taskto.ai/taskto/internal/drain"
)

// handlerWait is how long a shutdown waits for the handlers of a listener, after it has ended
// their contexts and closed their connections.
const handlerWait = 5 * time.Second

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
