package conversations

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sse is one Server-Sent Event, or comment, as a client reads it.
type sse struct {
	comment string
	data    string
	retry   string
}

// eventStream is an open GET …/events, read through a real HTTP connection.
type eventStream struct {
	t      *testing.T
	res    *http.Response
	r      *bufio.Reader
	cancel context.CancelFunc
}

// openEvents starts streaming c's conversation id from the server srv.
func (c *client) openEvents(srv *httptest.Server, ws, id int64) *eventStream {
	c.e.t.Helper()
	// Bounded, so a stream that should have ended and didn't fails a read instead of hanging the
	// test until the binary's timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	c.e.t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path(ws, fmt.Sprintf("/%d/events", id)), nil)
	if err != nil {
		c.e.t.Fatal(err)
	}
	req.AddCookie(c.cookie)
	res, err := srv.Client().Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	c.e.t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		c.e.t.Fatalf("events = %d", res.StatusCode)
	}
	s := &eventStream{t: c.e.t, res: res, r: bufio.NewReader(res.Body), cancel: cancel}
	if got := s.next(); got.retry != "2000" {
		c.e.t.Fatalf("first = %+v, want the retry delay", got)
	}
	return s
}

// next reads the next event or comment, failing the test at the end of the stream.
func (s *eventStream) next() sse {
	s.t.Helper()
	ev, err := s.read()
	if err != nil {
		s.t.Fatalf("reading the stream: %v", err)
	}
	return ev
}

func (s *eventStream) read() (sse, error) {
	var ev sse
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			return ev, err
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			return ev, nil
		case strings.HasPrefix(line, ": "):
			ev.comment = line[2:]
		case strings.HasPrefix(line, "data: "):
			if ev.data != "" {
				return ev, fmt.Errorf("an event with two data lines: %q", line)
			}
			ev.data = line[6:]
		case strings.HasPrefix(line, "retry: "):
			ev.retry = line[7:]
		default:
			return ev, fmt.Errorf("unexpected line %q", line)
		}
	}
}

// expectEnd reads the rest of the stream: data, then the end of the stream.
func (s *eventStream) expectEnd(data string) {
	s.t.Helper()
	if got := s.next(); got.data != data {
		s.t.Fatalf("event = %+v, want data %s", got, data)
	}
	if ev, err := s.read(); !errors.Is(err, io.EOF) {
		s.t.Fatalf("after the end: %+v, %v; want the end of the stream", ev, err)
	}
}

func newStreamEnv(t *testing.T) (*env, *httptest.Server, *client, int64, Conversation) {
	t.Helper()
	e := newEnv(t, false)
	srv := httptest.NewServer(e.h)
	t.Cleanup(srv.Close)
	owner, ws := e.setup()
	return e, srv, owner, ws, owner.create(ws, nil)
}

const interruptedLine = `{"type":"end","kind":"interrupted"}`

func TestEventsForwardEachLineAsAnEvent(t *testing.T) {
	e, srv, owner, ws, conv := newStreamEnv(t)
	s := owner.openEvents(srv, ws, conv.ID)
	if ct := s.res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := s.res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	w := e.rt.watch(t, 0)
	if w.conv != conv.agentIDForTest(t, e) {
		t.Fatalf("watching agent conversation %d", w.conv)
	}

	// Unchanged, including what the server doesn't know about.
	lines := []string{
		`{"type":"value","value":{"entries":[],"text":"a: b\n\nc"},"extra":1}`,
		`{"type":"ops","ops":[["set",["entries",0],{"x":" "}]]}`,
		`{"type":"ops","ops":[]}`,
	}
	for _, line := range lines {
		w.send(t, line)
		if got := s.next(); got.data != line {
			t.Fatalf("event = %+v, want data %s", got, line)
		}
	}
	end := `{"type":"end","kind":"stopped"}`
	w.send(t, end)
	s.expectEnd(end)
	waitFor(t, "the watch to close", w.isClosed)
}

func TestEventsHeartbeat(t *testing.T) {
	e, srv, owner, ws, conv := newStreamEnv(t)
	e.svc.heartbeat = 20 * time.Millisecond
	s := owner.openEvents(srv, ws, conv.ID)
	w := e.rt.watch(t, 0)
	for range 3 {
		if got := s.next(); got.comment != "heartbeat" || got.data != "" {
			t.Fatalf("event = %+v, want a heartbeat comment", got)
		}
	}
	// Events go on between heartbeats.
	w.send(t, `{"type":"value","value":{}}`)
	for {
		if got := s.next(); got.data != "" {
			break
		}
	}
}

func TestEventsClientLeavingClosesTheWatch(t *testing.T) {
	e, srv, owner, ws, conv := newStreamEnv(t)
	s := owner.openEvents(srv, ws, conv.ID)
	w := e.rt.watch(t, 0)
	w.send(t, `{"type":"value","value":{}}`)
	s.next()
	s.cancel()
	waitFor(t, "the watch to close", w.isClosed)
}

// The sidecar's stream ends without an end line when it restarts: the client is told, and
// reconnects for a fresh view.
func TestEventsAgentRuntimeGoneEndsTheStream(t *testing.T) {
	e, srv, owner, ws, conv := newStreamEnv(t)
	s := owner.openEvents(srv, ws, conv.ID)
	w := e.rt.watch(t, 0)
	w.send(t, `{"type":"value","value":{}}`)
	s.next()
	close(w.lines)
	s.expectEnd(interruptedLine)
	waitFor(t, "the watch to close", w.isClosed)

	// A reconnect gets a new watch.
	s = owner.openEvents(srv, ws, conv.ID)
	e.rt.watch(t, 1).send(t, `{"type":"value","value":{"fresh":true}}`)
	if got := s.next(); got.data != `{"type":"value","value":{"fresh":true}}` {
		t.Fatalf("after reconnecting: %+v", got)
	}
}

func TestEventsEndOnShutdown(t *testing.T) {
	e, srv, owner, ws, conv := newStreamEnv(t)
	s := owner.openEvents(srv, ws, conv.ID)
	w := e.rt.watch(t, 0)
	e.svc.EndStreams()
	s.expectEnd(interruptedLine)
	waitFor(t, "the watch to close", w.isClosed)
}

// Someone removed from the workspace loses an open stream at the next heartbeat.
func TestEventsEndWhenAccessEnds(t *testing.T) {
	e, srv, owner, ws, _ := newStreamEnv(t)
	member := e.invite(owner, ws, "member@example.com")
	conv := member.create(ws, nil)
	e.svc.heartbeat = 20 * time.Millisecond
	s := member.openEvents(srv, ws, conv.ID)
	w := e.rt.watch(t, 0)
	if got := s.next(); got.comment != "heartbeat" {
		t.Fatalf("event = %+v, want a heartbeat", got)
	}
	owner.expect(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, member.id), nil)
	for {
		got := s.next()
		if got.comment == "heartbeat" {
			continue
		}
		if got.data != interruptedLine {
			t.Fatalf("event = %+v, want the end", got)
		}
		break
	}
	waitFor(t, "the watch to close", w.isClosed)
}

// A member disabled while a stream is open loses it at the next heartbeat, as one removed does.
func TestEventsEndWhenOwnerDisabled(t *testing.T) {
	e, srv, owner, ws, _ := newStreamEnv(t)
	member := e.invite(owner, ws, "member@example.com")
	conv := member.create(ws, nil)
	e.svc.heartbeat = 20 * time.Millisecond
	s := member.openEvents(srv, ws, conv.ID)
	w := e.rt.watch(t, 0)
	if got := s.next(); got.comment != "heartbeat" {
		t.Fatalf("event = %+v, want a heartbeat", got)
	}
	owner.expect(http.StatusNoContent, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/disable", ws, member.id), nil)
	for {
		got := s.next()
		if got.comment == "heartbeat" {
			continue
		}
		if got.data != interruptedLine {
			t.Fatalf("event = %+v, want the end", got)
		}
		break
	}
	waitFor(t, "the watch to close", w.isClosed)
}

// A stream ends with the login that opened it, at once, even if the account is enabled again
// before the next heartbeat looks at it.
func TestEventsEndWithTheirLogin(t *testing.T) {
	e, srv, owner, ws, _ := newStreamEnv(t)
	member := e.invite(owner, ws, "member@example.com")
	conv := member.create(ws, nil)
	e.svc.heartbeat = time.Hour // the heartbeat's check can't be what ends it
	s := member.openEvents(srv, ws, conv.ID)
	w := e.rt.watch(t, 0)
	owner.expect(http.StatusNoContent, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/disable", ws, member.id), nil)
	owner.expect(http.StatusNoContent, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/enable", ws, member.id), nil)
	s.expectEnd(interruptedLine)
	waitFor(t, "the watch to close", w.isClosed)
}

// A stream watches its login once: each watch looks the session up on every heartbeat, so a
// second one would double the lookups of every open stream. It releases its watch when it ends.
func TestEventsWatchTheirSessionOnce(t *testing.T) {
	e, srv, owner, ws, conv := newStreamEnv(t)
	var started, stopped atomic.Int32
	watch := e.svc.watchSession
	e.svc.watchSession = func(r *http.Request, every time.Duration) (context.Context, context.CancelFunc) {
		started.Add(1)
		ctx, stop := watch(r, every)
		return ctx, func() {
			stopped.Add(1)
			stop()
		}
	}
	e.svc.heartbeat = 10 * time.Millisecond
	s := owner.openEvents(srv, ws, conv.ID)
	e.rt.watch(t, 0)
	// Several heartbeats in, the stream has still started one watch.
	for range 3 {
		if got := s.next(); got.comment != "heartbeat" {
			t.Fatalf("event = %+v, want a heartbeat", got)
		}
	}
	if n := started.Load(); n != 1 {
		t.Fatalf("the stream started %d session watches, want 1", n)
	}
	if n := stopped.Load(); n != 0 {
		t.Fatalf("%d session watches released while the stream is open", n)
	}
	owner.expect(http.StatusNoContent, http.MethodPost, "/api/logout", nil)
	for {
		if got := s.next(); got.data == interruptedLine {
			break
		}
	}
	waitFor(t, "the watch to be released", func() bool { return stopped.Load() == 1 })
}

// An open stream ends with the login that opened it: at once when the login is signed out
// (the heartbeat is too slow to be the cause), and at the next heartbeat when it ended some
// other way.
func TestEventsEndWhenTheLoginEnds(t *testing.T) {
	expectEnd := func(t *testing.T, s *eventStream) {
		t.Helper()
		for {
			got := s.next()
			if got.comment == "heartbeat" {
				continue
			}
			if got.data != interruptedLine {
				t.Fatalf("event = %+v, want the end", got)
			}
			return
		}
	}
	t.Run("signed out", func(t *testing.T) {
		e, srv, owner, ws, conv := newStreamEnv(t)
		e.svc.heartbeat = time.Hour
		s := owner.openEvents(srv, ws, conv.ID)
		w := e.rt.watch(t, 0)
		owner.expect(http.StatusNoContent, http.MethodPost, "/api/logout", nil)
		expectEnd(t, s)
		waitFor(t, "the watch to close", w.isClosed)
	})
	t.Run("revoked behind the signal", func(t *testing.T) {
		e, srv, owner, ws, conv := newStreamEnv(t)
		e.svc.heartbeat = 20 * time.Millisecond
		s := owner.openEvents(srv, ws, conv.ID)
		w := e.rt.watch(t, 0)
		if _, err := e.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, owner.id); err != nil {
			t.Fatal(err)
		}
		expectEnd(t, s)
		waitFor(t, "the watch to close", w.isClosed)
	})
}

// blockingWriter is a ResponseWriter whose writes wait until the test lets them through, like
// a client that has stopped reading.
type blockingWriter struct {
	header http.Header
	gate   chan struct{}
	mu     sync.Mutex
	writes int
}

func (b *blockingWriter) Header() http.Header { return b.header }
func (b *blockingWriter) WriteHeader(int)     {}
func (b *blockingWriter) Flush()              {}

func (b *blockingWriter) Write(p []byte) (int, error) {
	<-b.gate
	b.mu.Lock()
	b.writes++
	b.mu.Unlock()
	return len(p), nil
}

// A client that doesn't read holds back the sidecar's stream: nothing is read ahead of what
// the client has taken but the one event waiting to be written.
func TestEventsBackpressure(t *testing.T) {
	e, _, owner, ws, conv := newStreamEnv(t)
	e.svc.heartbeat = time.Hour
	b := &blockingWriter{header: http.Header{}, gate: make(chan struct{})}
	req := httptest.NewRequest(http.MethodGet, path(ws, fmt.Sprintf("/%d/events", conv.ID)), nil)
	req.AddCookie(owner.cookie)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.h.ServeHTTP(b, req)
	}()
	w := e.rt.watch(t, 0)

	b.gate <- struct{}{} // the retry line
	w.send(t, `{"type":"value","value":{}}`)
	b.gate <- struct{}{} // the value
	// The handler is now stuck writing the first ops line; it takes one more off the watch
	// and then stops reading.
	w.send(t, `{"type":"ops","ops":[1]}`)
	w.send(t, `{"type":"ops","ops":[2]}`)
	select {
	case w.lines <- `{"type":"ops","ops":[3]}`:
		t.Fatal("the handler read a third line while the client took none")
	case <-time.After(100 * time.Millisecond):
	}
	w.mu.Lock()
	nexts := w.nexts
	w.mu.Unlock()
	if nexts > 4 {
		t.Fatalf("Next called %d times for 3 lines", nexts)
	}
	// Letting the client read lets the stream go on.
	b.gate <- struct{}{}
	w.send(t, `{"type":"ops","ops":[3]}`)
	cancel()
	close(b.gate)
	<-done
	waitFor(t, "the watch to close", w.isClosed)
}

// A client that stops reading without leaving is dropped once one write has waited for
// writeTimeout, which closes the sidecar's watch.
func TestEventsStuckClientIsDropped(t *testing.T) {
	e, srv, owner, ws, conv := newStreamEnv(t)
	e.svc.writeTimeout = 200 * time.Millisecond
	owner.openEvents(srv, ws, conv.ID) // and never read again
	w := e.rt.watch(t, 0)
	big := `{"type":"ops","ops":["` + strings.Repeat("x", 1<<20) + `"]}`
	go func() {
		for {
			select {
			case w.lines <- big:
			case <-w.closed:
				return
			}
		}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for !w.isClosed() {
		if time.Now().After(deadline) {
			t.Fatal("the watch of a stuck client is still open")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// One user can hold only so many event streams; the next one is refused until one closes.
func TestEventsStreamsPerUserAreCapped(t *testing.T) {
	e, srv, owner, ws, conv := newStreamEnv(t)
	e.svc.maxStreams = 1
	first := owner.openEvents(srv, ws, conv.ID)
	e.rt.watch(t, 0)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path(ws, fmt.Sprintf("/%d/events", conv.ID)), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(owner.cookie)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second stream = %d, want 429", res.StatusCode)
	}

	first.cancel()
	waitFor(t, "the first stream to be released", func() bool {
		e.svc.mu.Lock()
		defer e.svc.mu.Unlock()
		return e.svc.streams[owner.id] == 0
	})
	owner.openEvents(srv, ws, conv.ID)
}

// A database error during the access check doesn't end a stream at once; one that goes on
// for several heartbeats does.
func TestEventsSurviveABriefDatabaseError(t *testing.T) {
	e, srv, owner, ws, conv := newStreamEnv(t)
	// The test is the stream's heartbeat clock: each tick runs one access check, and the
	// heartbeat comment that follows it, failed or not, says the check is done. Nothing
	// depends on how long anything takes. The interval is only the session watch's.
	e.svc.heartbeat = time.Hour
	ticks := make(chan time.Time)
	e.svc.heartbeatTicks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	s := owner.openEvents(srv, ws, conv.ID)
	e.rt.watch(t, 0)
	tick := func() {
		t.Helper()
		// A handler that returned without an end line has nobody left to take the tick: fail
		// instead of waiting for the test binary's timeout.
		select {
		case ticks <- time.Time{}:
		case <-time.After(10 * time.Second):
			t.Fatal("the stream took no heartbeat: it ended without an end event")
		}
	}
	check := func() sse {
		t.Helper()
		tick()
		return s.next()
	}
	rename := func(from, to string) {
		t.Helper()
		if _, err := e.db.Exec(`ALTER TABLE ` + from + ` RENAME TO ` + to); err != nil {
			t.Fatal(err)
		}
	}
	if got := check(); got.comment != "heartbeat" {
		t.Fatalf("event = %+v, want a heartbeat", got)
	}

	// A real database error for each of two checks, not enough to end the stream: the table
	// the check reads is gone.
	rename("conversations", "conversations_away")
	for range 2 {
		if got := check(); got.comment != "heartbeat" {
			t.Fatalf("event = %+v, want the stream to go on after a failed check", got)
		}
	}
	// A check that works again starts the count over: two more failures still don't end it.
	rename("conversations_away", "conversations")
	if got := check(); got.comment != "heartbeat" {
		t.Fatalf("event = %+v, want the stream to go on", got)
	}
	rename("conversations", "conversations_away")
	for range 2 {
		if got := check(); got.comment != "heartbeat" {
			t.Fatalf("event = %+v, want the stream to go on after the count started over", got)
		}
	}

	// For good: the third failed check in a row ends the stream.
	// The end event is the last thing on the connection: the handler returns after it.
	tick()
	s.expectEnd(interruptedLine)
}

// A line that can't be one event (it holds a line break) ends the stream with the end event,
// so the client reconnects at once instead of waiting on a silent connection.
func TestEventsBadLineEndsTheStreamCleanly(t *testing.T) {
	e, srv, owner, ws, conv := newStreamEnv(t)
	e.svc.heartbeat = time.Hour
	s := owner.openEvents(srv, ws, conv.ID)
	w := e.rt.watch(t, 0)
	w.send(t, "{\"type\":\n\"ops\"}") // valid JSON, but two lines
	s.expectEnd(interruptedLine)
}
