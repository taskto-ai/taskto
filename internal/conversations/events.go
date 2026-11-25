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

// interrupted is the end event the server sends itself when the stream stops for any reason
// other than the agent runtime ending it: the sidecar restarted or broke the stream, the
// caller lost access, or the server is shutting down. The client reconnects and starts again
// from a fresh view (or learns it can't).
var interrupted = []byte(`{"type":"end","kind":"interrupted"}`)

// handleEvents streams the agent runtime's watch of a conversation as Server-Sent Events. Each
// line of the sidecar's stream becomes one event's data, unchanged: first the view, then the
// Chord operations of each commit, which the browser applies with applyImmutable() from
// @earendil-works/chord/delta, and an end line when the sidecar ends it. The server checks
// access and copies lines; it doesn't read them.
//
// One event at a time is in flight between the sidecar and the client, so a slow client
// slows the sidecar's stream, which folds its commits while it waits. A client gone or
// stuck for writeTimeout on one write closes the sidecar's watch.
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
	// The stream lasts as long as the login that opened it: signing the login out, from anywhere,
	// ends it at once, and at the latest at the heartbeat's next look at the session.
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
	h.Set("X-Accel-Buffering", "no") // a reverse proxy mustn't hold events back
	w.WriteHeader(http.StatusOK)
	out := &eventWriter{w: w, rc: http.NewResponseController(w), timeout: s.writeTimeout}
	// The server has no write timeout; this connection's deadlines are set per write, and
	// cleared for whatever uses the connection next.
	defer func() { _ = out.rc.SetWriteDeadline(time.Time{}) }()
	// EventSource reconnects this long after a stream ends.
	if out.write([]byte("retry: 2000\n\n")) != nil {
		return
	}

	events := make(chan agentrt.Event) // unbuffered: the next line is read once this one is taken
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

	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()
	// A brief database error doesn't end the stream; losing access, or the check failing
	// several heartbeats in a row, does.
	const maxFailedChecks = 3
	failedChecks := 0
	for {
		select {
		case ev := <-events:
			if err := out.event(ev.Raw); err != nil {
				if errors.Is(err, errBadLine) {
					// Not the client's fault: say the stream ended, so it reconnects now.
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
			if r.Context().Err() == nil { // the client is still there
				_ = out.event(interrupted)
			}
			return
		case <-heartbeat.C:
			// Access is checked again, so someone removed from the workspace doesn't keep a stream.
			switch _, err := s.find(ctx, workspace, user, c.ID); {
			case errors.Is(err, errNotFound):
				_ = out.event(interrupted)
				return
			case err != nil:
				if ctx.Err() != nil {
					// The stream ended while the check ran (the login was signed out, or the server
					// is shutting down): say so as the end case does, so the client reconnects at once.
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
			// A comment: proxies see traffic on an idle stream and don't close it.
			if out.write([]byte(": heartbeat\n\n")) != nil {
				return
			}
		case <-ctx.Done():
			if r.Context().Err() == nil { // the server is shutting down, not the client gone
				_ = out.event(interrupted)
			}
			return
		}
	}
}

// eventWriter writes Server-Sent Events, each flushed at once.
type eventWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
}

// event writes data, one JSON line, as an event.
// errBadLine is for a line from the agent runtime that can't be one event.
var errBadLine = errors.New("conversations: line break in an event")

func (e *eventWriter) event(data []byte) error {
	// A JSON line has no raw line breaks; one with them would split the event.
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
