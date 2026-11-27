package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

type Recheck func(ctx context.Context) error

func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, topic Topic, user int64, recheck Recheck) {
	if h.cfg.WatchSession == nil {
		apierr.Internal(w)
		return
	}
	sessionCtx, stopWatch := h.cfg.WatchSession(r, h.cfg.Heartbeat)
	defer stopWatch()
	if sessionCtx.Err() != nil {
		apierr.Write(w, http.StatusUnauthorized, apierr.CodeNotLoggedIn, "log in first")
		return
	}
	ctx, cancel := context.WithCancel(sessionCtx)
	defer cancel()

	rc := http.NewResponseController(w)
	out := &eventWriter{w: w, rc: rc, timeout: h.cfg.WriteTimeout, ctx: ctx}
	defer out.close()
	sub, err := h.subscribe(topic, user, func() { cancel(); out.end() })
	switch {
	case errors.Is(err, ErrTooManyStreams):
		apierr.Write(w, http.StatusTooManyRequests, apierr.CodeTooManyStreams,
			fmt.Sprintf("too many open event streams (at most %d per user): close one first", h.cfg.MaxStreams),
			apierr.Params{"max": h.cfg.MaxStreams})
		return
	case errors.Is(err, ErrClosed):
		apierr.Write(w, http.StatusServiceUnavailable, apierr.CodeLiveClosed, "the server is shutting down")
		return
	case err != nil:
		apierr.Internal(w)
		return
	}
	defer h.unsubscribe(sub)
	events := sub.ch

	if err := recheck(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		var denied *apierr.Error
		if errors.As(err, &denied) {
			apierr.WriteError(w, denied)
			return
		}
		slog.ErrorContext(ctx, "live: checking access on connect", "kind", topic.Kind, "err", err)
		apierr.Internal(w)
		return
	}

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if out.deliver([]byte("retry: 2000\n\n"), nil) != nil {
		return
	}

	heartbeat, stopHeartbeat := h.ticks(h.cfg.Heartbeat)
	defer stopHeartbeat()
	rechecks, stopRechecks := h.ticks(h.cfg.Recheck)
	defer stopRechecks()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			var allowed func() bool
			if ev.carriesData() {
				allowed = func() bool { return stillAllowed(ctx, topic, recheck) }
			}
			if out.event(ev, allowed) != nil {
				return
			}
		case <-heartbeat:
			if out.deliver([]byte(": heartbeat\n\n"), nil) != nil {
				return
			}
		case <-rechecks:
			if !stillAllowed(ctx, topic, recheck) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func stillAllowed(ctx context.Context, topic Topic, recheck Recheck) bool {
	if err := recheck(ctx); err != nil {
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "live: ending a stream that failed its access check", "kind", topic.Kind, "err", err)
		}
		return false
	}
	return true
}

func (ev Event) carriesData() bool { return ev.Type != TypeReady && ev.Type != TypeResync }

var errStreamEnded = errors.New("live: the stream ended")

type eventWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
	ctx     context.Context

	mu       sync.Mutex
	ended    bool
	abortMu  sync.Mutex
	finished bool
}

func (e *eventWriter) event(ev Event, allowed func() bool) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return e.deliver(append(append([]byte("data: "), data...), '\n', '\n'), allowed)
}

func (e *eventWriter) deliver(p []byte, allowed func() bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ended || e.ctx.Err() != nil {
		return errStreamEnded
	}
	if err := e.rc.SetWriteDeadline(time.Now().Add(e.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if allowed != nil && !allowed() {
		return errStreamEnded
	}
	if e.ended || e.ctx.Err() != nil {
		return errStreamEnded
	}
	if _, err := e.w.Write(p); err != nil {
		return err
	}
	return e.rc.Flush()
}

func (e *eventWriter) end() {
	e.abortMu.Lock()
	if !e.finished {
		_ = e.rc.SetWriteDeadline(time.Now())
	}
	e.abortMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ended = true
}

func (e *eventWriter) close() {
	e.abortMu.Lock()
	defer e.abortMu.Unlock()
	e.finished = true
	_ = e.rc.SetWriteDeadline(time.Time{})
}
