package live

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/drain"
)

const (
	testHeartbeat = time.Second
	testRecheck   = 2 * time.Second
)

type clocks struct {
	mu   sync.Mutex
	tick map[time.Duration]chan time.Time
}

func newClocks() *clocks {
	return &clocks{tick: map[time.Duration]chan time.Time{testHeartbeat: make(chan time.Time), testRecheck: make(chan time.Time)}}
}

func (c *clocks) ticker(every time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tick[every], func() {}
}

func (c *clocks) fire(every time.Duration) {
	c.tick[every] <- time.Now()
}

type rig struct {
	t       *testing.T
	hub     *Hub
	clocks  *clocks
	srv     *httptest.Server
	recheck func(ctx context.Context) error

	mu      sync.Mutex
	session map[string]context.CancelFunc
}

func newRig(t *testing.T, cfg Config) *rig {
	t.Helper()
	g := &rig{t: t, clocks: newClocks(), session: map[string]context.CancelFunc{}}
	g.recheck = func(context.Context) error { return nil }
	cfg.Heartbeat, cfg.Recheck, cfg.Ticker = testHeartbeat, testRecheck, g.clocks.ticker
	cfg.WatchSession = g.watch
	g.hub = New(cfg)
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := int64(1)
		if r.URL.Query().Get("user") == "2" {
			user = 2
		}
		g.hub.Serve(w, r, room, user, func(ctx context.Context) error { return g.recheck(ctx) })
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *rig) watch(r *http.Request, _ time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	name := r.Header.Get("X-Session")
	if name == "" {
		cancel()
		return ctx, cancel
	}
	g.mu.Lock()
	g.session[name] = cancel
	g.mu.Unlock()
	return ctx, cancel
}

func (g *rig) publish(data string) {
	g.t.Helper()
	if _, err := g.hub.Publish(room, "stroke", json.RawMessage(data)); err != nil {
		g.t.Fatal(err)
	}
}

func (g *rig) endSession(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.session[name]()
}

type stream struct {
	t   *testing.T
	res *http.Response
	r   *bufio.Reader
}

func (g *rig) open(session, query string) *http.Response {
	g.t.Helper()
	req, err := http.NewRequest(http.MethodGet, g.srv.URL+"?"+query, nil)
	if err != nil {
		g.t.Fatal(err)
	}
	if session != "" {
		req.Header.Set("X-Session", session)
	}
	res, err := g.srv.Client().Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func (g *rig) connect(session string) *stream {
	g.t.Helper()
	res := g.open(session, "")
	g.t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		g.t.Fatalf("connect = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	s := &stream{t: g.t, res: res, r: bufio.NewReader(res.Body)}
	if line := s.line(); line != "retry: 2000" {
		g.t.Fatalf("first line = %q, want the retry delay", line)
	}
	s.line()
	if ev := s.event(); ev.Type != TypeReady {
		g.t.Fatalf("first event = %+v, want ready", ev)
	}
	return s
}

func (s *stream) line() string {
	s.t.Helper()
	line, err := s.r.ReadString('\n')
	if err != nil {
		s.t.Fatalf("reading the stream: %v", err)
	}
	return strings.TrimSuffix(line, "\n")
}

func (s *stream) event() Event {
	s.t.Helper()
	for {
		line := s.line()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		raw, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			s.t.Fatalf("line %q is not data", line)
		}
		var ev Event
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			s.t.Fatal(err)
		}
		if blank := s.line(); blank != "" {
			s.t.Fatalf("an event ended with %q, want an empty line", blank)
		}
		return ev
	}
}

func (s *stream) ended() {
	s.t.Helper()
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := s.r.ReadString('\n'); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			s.t.Fatalf("the stream ended with %v, want EOF", err)
		}
	case <-time.After(5 * time.Second):
		s.t.Fatal("the stream stayed open")
	}
}

func (s *stream) endedWithoutData() {
	s.t.Helper()
	done := make(chan []string, 1)
	go func() {
		var data []string
		for {
			line, err := s.r.ReadString('\n')
			if strings.HasPrefix(line, "data: ") {
				data = append(data, line)
			}
			if err != nil {
				done <- data
				return
			}
		}
	}()
	select {
	case data := <-done:
		if len(data) != 0 {
			s.t.Fatalf("the stream wrote %q after its access ended", data)
		}
	case <-time.After(5 * time.Second):
		s.t.Fatal("the stream stayed open")
	}
}

func (g *rig) refused(session, query string) (int, apierr.Code, map[string]any) {
	g.t.Helper()
	res := g.open(session, query)
	defer func() { _ = res.Body.Close() }()
	code, params := errorBody(g.t, res)
	return res.StatusCode, code, params
}

func errorBody(t *testing.T, res *http.Response) (apierr.Code, map[string]any) {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	var b struct {
		Code   apierr.Code    `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.NewDecoder(res.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	return b.Code, b.Params
}

func (g *rig) waitSubscribers(n int) {
	g.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for g.hub.openStreams() != n {
		if time.Now().After(deadline) {
			g.t.Fatalf("%d subscribers, want %d", g.hub.openStreams(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAStreamCarriesPublishedEventsInOrder(t *testing.T) {
	g := newRig(t, Config{})
	a, b := g.connect("a"), g.connect("b")
	g.publish(`{"x":1}`)
	g.publish(`{"x":"two\nlines"}`)
	for _, s := range []*stream{a, b} {
		if ev := s.event(); ev.Seq != 1 || ev.Type != "stroke" || string(ev.Data) != `{"x":1}` {
			t.Fatalf("first = %+v", ev)
		}
		if ev := s.event(); ev.Seq != 2 || string(ev.Data) != `{"x":"two\nlines"}` {
			t.Fatalf("second = %+v", ev)
		}
	}
}

func TestAHeartbeatKeepsAnIdleStreamAlive(t *testing.T) {
	g := newRig(t, Config{})
	s := g.connect("a")
	go g.clocks.fire(testHeartbeat)
	if line := s.line(); line != ": heartbeat" {
		t.Fatalf("line = %q", line)
	}
}

func TestAStreamNeedsALogin(t *testing.T) {
	g := newRig(t, Config{})
	if status, code, _ := g.refused("", ""); status != http.StatusUnauthorized || code != apierr.CodeNotLoggedIn {
		t.Fatalf("no session = %d %q", status, code)
	}
	if n := g.hub.openStreams(); n != 0 {
		t.Fatalf("%d streams for a request with no login", n)
	}
}

func TestEndingTheSessionEndsTheStreamAndFreesItsSlot(t *testing.T) {
	g := newRig(t, Config{})
	s := g.connect("a")
	other := g.connect("b")
	g.endSession("a")
	s.ended()
	g.waitSubscribers(1)
	g.publish("")
	if ev := other.event(); ev.Seq != 1 {
		t.Fatalf("the other session = %+v", ev)
	}
}

func TestARefusedCheckOnConnectOpensNoStream(t *testing.T) {
	g := newRig(t, Config{})
	g.recheck = func(context.Context) error {
		return apierr.New(http.StatusNotFound, apierr.CodeSpaceNotFound, "space not found")
	}
	if status, code, _ := g.refused("a", ""); status != http.StatusNotFound || code != apierr.CodeSpaceNotFound {
		t.Fatalf("refused = %d %q", status, code)
	}
	g.waitSubscribers(0)

	g.recheck = func(context.Context) error { return errors.New("the database is down") }
	if status, code, _ := g.refused("a", ""); status != http.StatusInternalServerError || code != apierr.CodeInternal {
		t.Fatalf("failed = %d %q", status, code)
	}
	g.waitSubscribers(0)
}

func TestTheCheckRunsAgainWhileConnectedAndAFailureEndsTheStream(t *testing.T) {
	g := newRig(t, Config{})
	var mu sync.Mutex
	var calls int
	var failing bool
	g.recheck = func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if failing {
			return errors.New("access lost")
		}
		return nil
	}
	s := g.connect("a")
	g.clocks.fire(testRecheck)
	g.publish("")
	if ev := s.event(); ev.Seq != 1 {
		t.Fatalf("after a passing check = %+v", ev)
	}
	mu.Lock()
	if calls != 3 {
		t.Fatalf("%d checks, want the one on connect, the tick and the one before the event", calls)
	}
	failing = true
	mu.Unlock()
	g.clocks.fire(testRecheck)
	s.ended()
	g.waitSubscribers(0)
}

func TestTooManyStreamsAreRefusedWithItsCodeAndCountedAcrossConnections(t *testing.T) {
	g := newRig(t, Config{MaxStreams: 2})
	g.connect("a")
	g.connect("b")
	status, code, params := g.refused("c", "")
	if status != http.StatusTooManyRequests || code != apierr.CodeTooManyStreams || params["max"] != float64(2) {
		t.Fatalf("third = %d %q %v", status, code, params)
	}
	other := g.open("c", "user=2")
	g.t.Cleanup(func() { _ = other.Body.Close() })
	if other.StatusCode != http.StatusOK {
		t.Fatalf("another user = %d", other.StatusCode)
	}
}

func TestAStreamThatFellBehindIsToldToResyncAndEnds(t *testing.T) {
	g := newRig(t, Config{Buffer: 2})
	holding, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var armed atomic.Bool
	g.recheck = func(context.Context) error {
		if armed.Load() {
			once.Do(func() {
				close(holding)
				<-release
			})
		}
		return nil
	}
	s := g.connect("a")
	armed.Store(true)
	g.publish("")
	<-holding
	for range 3 {
		g.publish("")
	}
	close(release)
	var last Event
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			break
		}
		if raw, ok := strings.CutPrefix(line, "data: "); ok {
			_ = json.Unmarshal([]byte(raw), &last)
		}
	}
	if last.Type != TypeResync {
		t.Fatalf("last event = %+v, want a resync before the stream ended", last)
	}
}

func TestShuttingDownEndsOpenStreamsSoTheServerStopsAndLaterOnesAreRefused(t *testing.T) {
	g := newRig(t, Config{})
	inflight := drain.New()
	srv := &http.Server{Handler: inflight.Handler(g.srv.Config.Handler), ReadHeaderTimeout: time.Second}
	srv.RegisterOnShutdown(g.hub.EndStreams)
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = srv
	ts.Start()
	t.Cleanup(ts.Close)

	open := func() (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
		req.Header.Set("X-Session", "a")
		return ts.Client().Do(req)
	}
	res, err := open()
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("open = %v %v", res, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	s := &stream{t: t, res: res, r: bufio.NewReader(res.Body)}
	s.line()
	s.line()
	s.event()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown with a stream open: %v", err)
	}
	inflight.Stop()
	if err := inflight.Wait(5 * time.Second); err != nil {
		t.Fatalf("the drain did not finish: %v", err)
	}
	s.ended()

	after := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Session", "a")
	g.srv.Config.Handler.ServeHTTP(after, r)
	if code, _ := errorBody(t, after.Result()); after.Code != http.StatusServiceUnavailable || code != apierr.CodeLiveClosed {
		t.Fatalf("after the end = %d %q", after.Code, code)
	}
}

func TestAHubWithNoSessionWatcherServesNothing(t *testing.T) {
	h := New(Config{})
	rec := httptest.NewRecorder()
	h.Serve(rec, httptest.NewRequest(http.MethodGet, "/", nil), room, 1, func(context.Context) error { return nil })
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if n := h.openStreams(); n != 0 {
		t.Fatalf("%d streams", n)
	}
}

func TestAnEventIsNotWrittenToAStreamWhoseAccessNoLongerHolds(t *testing.T) {
	g := newRig(t, Config{})
	s := g.connect("a")
	g.recheck = func(context.Context) error {
		return apierr.New(http.StatusNotFound, apierr.CodeSpaceNotFound, "space not found")
	}
	g.publish(`{"secret":1}`)
	s.endedWithoutData()
	g.waitSubscribers(0)
}

type gatedWriter struct {
	mu     sync.Mutex
	header http.Header
	body   bytes.Buffer
	first  chan struct{}
	gate   chan struct{}
	once   sync.Once
}

func newGatedWriter() *gatedWriter {
	return &gatedWriter{header: http.Header{}, first: make(chan struct{}), gate: make(chan struct{})}
}

func (w *gatedWriter) Header() http.Header { return w.header }
func (w *gatedWriter) WriteHeader(int)     {}
func (w *gatedWriter) Flush()              {}

func (w *gatedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() {
		close(w.first)
		<-w.gate
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}

func (w *gatedWriter) waitFor(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(w.written(), text) {
		if time.Now().After(deadline) {
			t.Fatalf("%q was never written", text)
		}
		time.Sleep(time.Millisecond)
	}
}

func (w *gatedWriter) written() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func serveGated(g *rig, w http.ResponseWriter, session string, recheck Recheck) <-chan struct{} {
	done := make(chan struct{})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Session", session)
	go func() {
		defer close(done)
		g.hub.Serve(w, r, room, 1, recheck)
	}()
	return done
}

func finished(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return")
	}
}

func TestARevokedStreamWritesNoQueuedEvent(t *testing.T) {
	g := newRig(t, Config{})
	w := newGatedWriter()
	done := serveGated(g, w, "a", func(context.Context) error { return nil })
	<-w.first
	g.publish(`{"queued":1}`)
	hook := make(chan struct{})
	go func() {
		defer close(hook)
		g.hub.EndUserIn(room.SpaceID, 1)
	}()
	g.waitRevoked(1)
	close(w.gate)
	finished(t, done)
	finished(t, hook)
	if got := w.written(); strings.Contains(got, "data:") {
		t.Fatalf("wrote %q after the revocation", got)
	}
}

func TestARevocationAfterAnEventWasTakenStopsItsWrite(t *testing.T) {
	g := newRig(t, Config{})
	w := newGatedWriter()
	close(w.gate)
	checking, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	done := serveGated(g, w, "a", func(ctx context.Context) error {
		if w.written() != "" {
			once.Do(func() { close(checking) })
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return nil
	})
	<-w.first
	g.waitSubscribers(1)
	for !strings.Contains(w.written(), "ready") {
		time.Sleep(time.Millisecond)
	}
	g.publish(`{"taken":1}`)
	<-checking
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		g.hub.EndUserIn(room.SpaceID, 1)
	}()
	t.Cleanup(func() { close(release) })
	finished(t, returned)
	finished(t, done)
	if got := w.written(); strings.Contains(got, "taken") {
		t.Fatalf("wrote %q after the revocation", got)
	}
}

func TestAnEndedSessionWritesNoEventThatWasAlreadyTaken(t *testing.T) {
	g := newRig(t, Config{})
	w := newGatedWriter()
	close(w.gate)
	checking, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	done := serveGated(g, w, "a", func(context.Context) error {
		if w.written() != "" {
			once.Do(func() { close(checking) })
			<-release
		}
		return nil
	})
	<-w.first
	for !strings.Contains(w.written(), "ready") {
		time.Sleep(time.Millisecond)
	}
	g.publish(`{"taken":1}`)
	<-checking
	g.endSession("a")
	close(release)
	finished(t, done)
	if got := w.written(); strings.Contains(got, "taken") {
		t.Fatalf("wrote %q after the session ended", got)
	}
}

func TestAStreamKeepsItsSlotUntilItsHandlerReturns(t *testing.T) {
	g := newRig(t, Config{MaxStreams: 1, Buffer: 2})
	w := newGatedWriter()
	done := serveGated(g, w, "a", func(context.Context) error { return nil })
	<-w.first
	for range 3 {
		g.publish(`1`)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	r.Header.Set("X-Session", "b")
	g.srv.Config.Handler.ServeHTTP(rec, r)
	if code, _ := errorBody(t, rec.Result()); rec.Code != http.StatusTooManyRequests || code != apierr.CodeTooManyStreams {
		t.Fatalf("a second stream while the first handler is still open = %d %q, want 429", rec.Code, code)
	}
	close(w.gate)
	finished(t, done)
	g.waitSubscribers(0)
}

func TestAClientThatOverflowsBeforeItsFirstEventStillGetsReadyThenResync(t *testing.T) {
	g := newRig(t, Config{Buffer: 3})
	w := newGatedWriter()
	done := serveGated(g, w, "a", func(context.Context) error { return nil })
	<-w.first
	for range 5 {
		g.publish(`1`)
	}
	close(w.gate)
	finished(t, done)
	var types []string
	for _, line := range strings.Split(w.written(), "\n") {
		if raw, ok := strings.CutPrefix(line, "data: "); ok {
			var ev Event
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				t.Fatal(err)
			}
			types = append(types, ev.Type)
		}
	}
	if strings.Join(types, ",") != "ready,resync" {
		t.Fatalf("the client saw %v, want ready then resync", types)
	}
}

func (h *Hub) revokedStreams() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for s := range h.all {
		if s.detached {
			n++
		}
	}
	return n
}

func (g *rig) waitRevoked(n int) {
	g.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for g.hub.revokedStreams() != n {
		if time.Now().After(deadline) {
			g.t.Fatalf("%d revoked streams, want %d", g.hub.revokedStreams(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

type pausingWriter struct {
	*gatedWriter
	armed      atomic.Bool
	armedWrite atomic.Bool
	paused     chan struct{}
	resume     chan struct{}
	release    sync.Once
}

func newPausingWriter(t *testing.T) *pausingWriter {
	w := &pausingWriter{gatedWriter: newGatedWriter(), paused: make(chan struct{}), resume: make(chan struct{})}
	close(w.gate)
	t.Cleanup(w.letGo)
	return w
}

func (w *pausingWriter) letGo() { w.release.Do(func() { close(w.resume) }) }

func (w *pausingWriter) Write(p []byte) (int, error) {
	if w.armedWrite.CompareAndSwap(true, false) {
		close(w.paused)
		<-w.resume
	}
	return w.gatedWriter.Write(p)
}

func (w *pausingWriter) SetWriteDeadline(t time.Time) error {
	if !t.IsZero() && time.Until(t) > time.Second && w.armed.CompareAndSwap(true, false) {
		close(w.paused)
		<-w.resume
	}
	return nil
}

func TestNothingIsWrittenOnceARevocationHasReturnedEvenForAWriteThatWasWaitingToStart(t *testing.T) {
	hooks := map[string]func(*rig){
		"a member removed from the space": func(g *rig) { g.hub.EndUserIn(room.SpaceID, 1) },
		"the space ended":                 func(g *rig) { g.hub.EndSpace(room.SpaceID) },
		"shutdown":                        func(g *rig) { g.hub.EndStreams() },
	}
	for name, hook := range hooks {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, Config{})
			w := newPausingWriter(t)
			var seen atomic.Pointer[context.Context]
			done := serveGated(g, w, "a", func(ctx context.Context) error {
				seen.Store(&ctx)
				return nil
			})
			<-w.first
			w.waitFor(t, "ready")
			w.armed.Store(true)
			g.publish(`{"waiting":1}`)
			<-w.paused
			returned := make(chan struct{})
			go func() {
				defer close(returned)
				hook(g)
			}()
			select {
			case <-returned:
			case <-(*seen.Load()).Done():
			case <-time.After(5 * time.Second):
				t.Fatal("the revocation neither returned nor reached the stream")
			}
			w.letGo()
			finished(t, returned)
			finished(t, done)
			if got := w.written(); strings.Contains(got, "waiting") {
				t.Fatalf("wrote %q after the revocation returned", got)
			}
		})
	}
}

func TestARevocationEndsAResourceCheckThatIsRunning(t *testing.T) {
	hooks := map[string]func(*rig){
		"a member removed from the space": func(g *rig) { g.hub.EndUserIn(room.SpaceID, 1) },
		"shutdown":                        func(g *rig) { g.hub.EndStreams() },
	}
	for name, hook := range hooks {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, Config{})
			w := newGatedWriter()
			close(w.gate)
			checking := make(chan struct{})
			var once sync.Once
			done := serveGated(g, w, "a", func(ctx context.Context) error {
				if w.written() == "" {
					return nil
				}
				once.Do(func() { close(checking) })
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(10 * time.Second):
					return nil
				}
			})
			<-w.first
			w.waitFor(t, "ready")
			g.publish(`{"checking":1}`)
			<-checking
			returned := make(chan struct{})
			go func() {
				defer close(returned)
				hook(g)
			}()
			finished(t, done)
			finished(t, returned)
			if got := w.written(); strings.Contains(got, "checking") {
				t.Fatalf("wrote %q after the revocation", got)
			}
		})
	}
}

func TestACancelledSessionKeepsTheSlotUntilTheHandlerReturns(t *testing.T) {
	g := newRig(t, Config{MaxStreams: 1})
	w := newGatedWriter()
	done := serveGated(g, w, "a", func(context.Context) error { return nil })
	<-w.first
	g.endSession("a")
	deadline := time.Now().Add(200 * time.Millisecond)
	for g.hub.openStreams() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	second := g.open("b", "")
	defer func() { _ = second.Body.Close() }()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a second stream while the first handler is still open = %d, want 429", second.StatusCode)
	}
	if code, _ := errorBody(t, second); code != apierr.CodeTooManyStreams {
		t.Fatalf("code = %q", code)
	}
	close(w.gate)
	finished(t, done)
	g.waitSubscribers(0)
}

func TestARevocationWaitsForAWriteThatIsAlreadyInFlight(t *testing.T) {
	g := newRig(t, Config{})
	w := newPausingWriter(t)
	var seen atomic.Pointer[context.Context]
	done := serveGated(g, w, "a", func(ctx context.Context) error {
		seen.Store(&ctx)
		return nil
	})
	<-w.first
	w.waitFor(t, "ready")
	w.armedWrite.Store(true)
	g.publish(`{"inflight":1}`)
	<-w.paused
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		g.hub.EndUserIn(room.SpaceID, 1)
	}()
	select {
	case <-(*seen.Load()).Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the revocation never reached the stream")
	}
	select {
	case <-returned:
		t.Fatal("the revocation returned while a write was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	w.letGo()
	finished(t, returned)
	finished(t, done)
	if got := w.written(); !strings.Contains(got, "inflight") {
		t.Fatalf("the write that had started was lost: %q", got)
	}
}
