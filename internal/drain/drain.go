// Package drain tracks the requests a server is handling, so that shutting down can wait for
// all of them. http.Server.Shutdown and Close don't: a handler on a connection that was closed,
// or hijacked, goes on running, and what it does with the database after the server's owner has
// closed it fails or, worse, races the close.
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

// ErrBusy is returned when handlers were still running after the time allowed for them.
var ErrBusy = errors.New("drain: handlers still running")

// Tracker counts the requests in flight on the servers whose handlers it wraps. The zero value
// is not usable: use New.
type Tracker struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	active int
	closed bool
	idle   chan struct{} // closed when active is 0 and the tracker is closed
}

// New returns a tracker that admits requests.
func New() *Tracker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Tracker{ctx: ctx, cancel: cancel, idle: make(chan struct{})}
}

// Handler returns h counting each request while it runs. After the tracker is stopped it
// answers 503 without calling h, and when the tracker is stopped it ends the context of each
// request still running, so a handler that honours its context returns promptly.
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

// Stop admits no more requests and ends the context of those running. It may be called more
// than once.
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

// Wait waits for the requests running when the tracker was stopped, for at most wait. It
// returns ErrBusy, with their number, if some are still running. Call Stop first.
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

// Shutdown stops srv and returns only when none of the handlers its Handler wrapped is running,
// or wait has passed since the connections were closed (ErrBusy then). srv first gets grace to
// finish its requests (none if grace is 0); whether that worked or not (running out of grace is
// not an error), the requests' contexts
// are then ended, the connections that remain are closed and the handlers are waited for.
// A hijacked connection is the handler's own, so only waiting for the handler covers it.
func (t *Tracker) Shutdown(srv *http.Server, grace, wait time.Duration) error {
	var err error
	if grace > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), grace)
		err = srv.Shutdown(ctx)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			// Requests that outlast the grace are ended below; that is not a failure.
			slog.Warn("drain: requests did not finish within the grace period; ending them", "grace", grace)
			err = nil
		}
	}
	t.Stop()
	err = errors.Join(err, srv.Close())
	return errors.Join(err, t.Wait(wait))
}
