package main

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"taskto.ai/taskto/internal/drain"
)

var handlerWait = defaultHandlerWait

const (
	defaultShutdownGrace = 10 * time.Second
	defaultHandlerWait   = 5 * time.Second
	emailWait            = 10 * time.Second
	proxyGrace           = 5 * time.Second
	schedulerWait        = 10 * time.Second
	sidecarStopWait      = 20 * time.Second

	shutdownBudget = defaultShutdownGrace + defaultHandlerWait + emailWait +
		proxyGrace + defaultHandlerWait + schedulerWait + sidecarStopWait
)

type shutdownReport struct {
	errs []error
	busy bool
}

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
