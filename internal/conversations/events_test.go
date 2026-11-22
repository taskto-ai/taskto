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
	ctx, cancel := context.WithCancel(context.Background())
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
	// Each check is followed by a heartbeat comment, failed or not, so counting comments
	// counts checks; the interval leaves room to restore the table between two of them.
	e.svc.heartbeat = 100 * time.Millisecond
	s := owner.openEvents(srv, ws, conv.ID)
	e.rt.watch(t, 0)
	if got := s.next(); got.comment != "heartbeat" {
		t.Fatalf("event = %+v, want a heartbeat", got)
	}

	// A real database error for one check: the table the check reads is gone.
	if _, err := e.db.Exec(`ALTER TABLE conversations RENAME TO conversations_away`); err != nil {
		t.Fatal(err)
	}
	if got := s.next(); got.comment != "heartbeat" {
		t.Fatalf("event = %+v, want the stream to go on after one failed check", got)
	}
	if _, err := e.db.Exec(`ALTER TABLE conversations_away RENAME TO conversations`); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if got := s.next(); got.comment != "heartbeat" {
			t.Fatalf("event = %+v, want the stream to go on", got)
		}
	}

	// For good: the stream ends after a few failed checks.
	if _, err := e.db.Exec(`ALTER TABLE conversations RENAME TO conversations_away`); err != nil {
		t.Fatal(err)
	}
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
