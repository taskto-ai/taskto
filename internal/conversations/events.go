package conversations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/apierr"
)

var interrupted = []byte(`{"type":"end","kind":"interrupted"}`)

func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conversationInPath(w, r)
	if !ok {
		return
	}
	rt, ok := s.runtime(w, r)
	if !ok {
		return
	}
	user, workspace := caller(r)
	sessionCtx, stopWatch := s.watchSession(r, s.heartbeat)
	defer stopWatch()
	if sessionCtx.Err() != nil {
		writeError(w, http.StatusUnauthorized, apierr.CodeNotLoggedIn, "log in first")
		return
	}
	closeStream, ok := s.openStream(user)
	if !ok {
		writeError(w, http.StatusTooManyRequests, apierr.CodeTooManyStreams,
			fmt.Sprintf("too many open event streams (at most %d per user): close one first", s.maxStreams), apierr.Params{"max": s.maxStreams})
		return
	}
	defer closeStream()
	ctx, cancel := context.WithCancel(sessionCtx)
	defer cancel()
	defer context.AfterFunc(s.closing, cancel)()
	watch, err := rt.Watch(ctx, c.agentID)
	if err != nil {
		conversationError(w, r, c, err)
		return
	}
	defer watch.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	out := &eventWriter{w: w, rc: http.NewResponseController(w), timeout: s.writeTimeout}
	defer func() { _ = out.rc.SetWriteDeadline(time.Time{}) }()
	if out.write([]byte("retry: 2000\n\n")) != nil {
		return
	}

	events := make(chan agentrt.Event)
	failed := make(chan error, 1)
	go func() {
		for {
			ev, err := watch.Next()
			if err != nil {
				failed <- err
				return
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	heartbeat, stopHeartbeat := s.heartbeatClock()
	defer stopHeartbeat()
	const maxFailedChecks = 3
	failedChecks := 0
	for {
		select {
		case ev := <-events:
			if err := out.event(ev.Raw); err != nil {
				if errors.Is(err, errBadLine) {
					slog.WarnContext(ctx, "conversations: the agent runtime sent a line with a line break", "conversation", c.ID)
					_ = out.event(interrupted)
				}
				return
			}
			if ev.Type == "end" {
				return
			}
		case err := <-failed:
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				slog.WarnContext(ctx, "conversations: event stream broke", "conversation", c.ID, "err", err)
			}
			if r.Context().Err() == nil {
				_ = out.event(interrupted)
			}
			return
		case <-heartbeat:
			switch _, err := s.reach(ctx, workspace, user, c.ID); {
			case errors.Is(err, errNotFound):
				_ = out.event(interrupted)
				return
			case err != nil:
				if ctx.Err() != nil {
					if r.Context().Err() == nil {
						_ = out.event(interrupted)
					}
					return
				}
				failedChecks++
				slog.WarnContext(ctx, "conversations: checking a stream's access", "conversation", c.ID, "err", err, "failed_checks", failedChecks)
				if failedChecks >= maxFailedChecks {
					_ = out.event(interrupted)
					return
				}
			default:
				failedChecks = 0
			}
			if out.write([]byte(": heartbeat\n\n")) != nil {
				return
			}
		case <-ctx.Done():
			if r.Context().Err() == nil {
				_ = out.event(interrupted)
			}
			return
		}
	}
}

func (s *Service) heartbeatClock() (<-chan time.Time, func()) {
	if s.heartbeatTicks != nil {
		return s.heartbeatTicks()
	}
	t := time.NewTicker(s.heartbeat)
	return t.C, t.Stop
}

type eventWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
}

var errBadLine = errors.New("conversations: line break in an event")

func (e *eventWriter) event(data []byte) error {
	if bytes.ContainsAny(data, "\r\n") {
		return errBadLine
	}
	buf := make([]byte, 0, len(data)+8)
	buf = append(buf, "data: "...)
	buf = append(buf, data...)
	return e.write(append(buf, '\n', '\n'))
}

func (e *eventWriter) write(p []byte) error {
	if err := e.rc.SetWriteDeadline(time.Now().Add(e.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := e.w.Write(p); err != nil {
		return err
	}
	return e.rc.Flush()
}
