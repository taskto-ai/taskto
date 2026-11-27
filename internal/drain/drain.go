package drain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

var ErrBusy = errors.New("drain: handlers still running")

type Tracker struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	active int
	closed bool
	idle   chan struct{}
}

func New() *Tracker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Tracker{ctx: ctx, cancel: cancel, idle: make(chan struct{})}
}

func (t *Tracker) Handler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !t.enter() {
			w.Header().Set("Connection", "close")
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		defer t.leave()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		defer context.AfterFunc(t.ctx, cancel)()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (t *Tracker) enter() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.active++
	return true
}

func (t *Tracker) leave() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active--
	if t.closed && t.active == 0 {
		close(t.idle)
	}
}

func (t *Tracker) Stop() {
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		if t.active == 0 {
			close(t.idle)
		}
	}
	t.mu.Unlock()
	t.cancel()
}

func (t *Tracker) Wait(wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-t.idle:
		return nil
	case <-timer.C:
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active == 0 {
		return nil
	}
	return fmt.Errorf("%w: %d after %s", ErrBusy, t.active, wait)
}

func (t *Tracker) Shutdown(srv *http.Server, grace, wait time.Duration) error {
	var err error
	if grace > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), grace)
		err = srv.Shutdown(ctx)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("drain: requests did not finish within the grace period; ending them", "grace", grace)
			err = nil
		}
	}
	t.Stop()
	err = errors.Join(err, srv.Close())
	return errors.Join(err, t.Wait(wait))
}
