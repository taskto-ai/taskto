package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

	events, revoked, unsubscribe, err := h.Subscribe(ctx, topic, user)
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
	defer unsubscribe()

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
	out := &eventWriter{w: w, rc: http.NewResponseController(w), timeout: h.cfg.WriteTimeout, ctx: ctx, revoked: revoked}
	defer func() { _ = out.rc.SetWriteDeadline(time.Time{}) }()
	if out.write([]byte("retry: 2000\n\n")) != nil {
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
			if ev.carriesData() && !stillAllowed(ctx, topic, recheck) {
				return
			}
			if out.event(ev) != nil {
				return
			}
		case <-heartbeat:
			if out.write([]byte(": heartbeat\n\n")) != nil {
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
	revoked <-chan struct{}
}

func (e *eventWriter) event(ev Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return e.write(append(append([]byte("data: "), data...), '\n', '\n'))
}

func (e *eventWriter) write(p []byte) error {
	select {
	case <-e.revoked:
		return errStreamEnded
	default:
	}
	if e.ctx.Err() != nil {
		return errStreamEnded
	}
	if err := e.rc.SetWriteDeadline(time.Now().Add(e.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := e.w.Write(p); err != nil {
		return err
	}
	return e.rc.Flush()
}
