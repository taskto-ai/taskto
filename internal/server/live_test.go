package server

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/live"
)

type liveStack struct {
	*stack
	checker *access.Checker
	hub     *live.Hub
	base    string

	mu       sync.Mutex
	rechecks []chan time.Time

	letGo  func()
	armed  atomic.Bool
	paused chan struct{}
	resume chan struct{}
}

func newLiveStack(t *testing.T) *liveStack {
	t.Helper()
	l := &liveStack{paused: make(chan struct{}), resume: make(chan struct{})}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(l.resume) }) })
	l.letGo = func() { release.Do(func() { close(l.resume) }) }
	var accounts *auth.Service
	var sqlDB *sql.DB
	l.hub = live.New(live.Config{
		Heartbeat: time.Hour, Recheck: 2 * time.Hour,
		Ticker: func(every time.Duration) (<-chan time.Time, func()) {
			if every != 2*time.Hour {
				return nil, func() {}
			}
			tick := make(chan time.Time)
			l.mu.Lock()
			l.rechecks = append(l.rechecks, tick)
			l.mu.Unlock()
			return tick, func() {}
		},
	})
	mount := func(inner http.Handler) http.Handler {
		checker := access.New(sqlDB, accounts)
		l.checker = checker
		const room = "/api/workspaces/{workspace}/spaces/{space}/rooms/{room}"
		mux := http.NewServeMux()
		mux.Handle("GET "+room+"/live", checker.RequireSpace(access.Read, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sp, _ := access.SpaceFrom(r.Context())
			u, _ := auth.UserFrom(r.Context())
			n, _ := strconv.ParseInt(r.PathValue("room"), 10, 64)
			check := live.RecheckSpace(checker, u.ID, sp)
			if r.URL.Query().Get("lenient") != "" {
				check = func(context.Context) error { return nil }
			}
			recheck := func(ctx context.Context) error {
				if l.armed.CompareAndSwap(true, false) {
					close(l.paused)
					<-l.resume
				}
				return check(ctx)
			}
			l.hub.Serve(w, r, live.Topic{Kind: "room", SpaceID: sp.ID, Generation: sp.Generation, ResourceID: n}, u.ID, recheck)
		})))
		mux.Handle("POST "+room+"/events", auth.CSRF(checker.RequireSpace(access.Write, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sp, _ := access.SpaceFrom(r.Context())
			n, _ := strconv.ParseInt(r.PathValue("room"), 10, 64)
			if _, err := l.hub.Publish(live.Topic{Kind: "room", SpaceID: sp.ID, Generation: sp.Generation, ResourceID: n}, "stroke", json.RawMessage(`{}`)); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusNoContent)
		}))))
		mux.Handle("/", inner)
		return mux
	}
	l.stack = newStackDB(t, func(o *Options, db *sql.DB) {
		sqlDB = db
		o.Live = l.hub
		o.AccountsReady = func(a *auth.Service) { accounts = a }
	}, mount)
	l.base = strings.TrimSuffix(l.mcpURL, "/mcp")
	return l
}

func (l *liveStack) path(space int64, tail string) string {
	return fmt.Sprintf("%s/api/workspaces/%d/spaces/%d/rooms/7/%s", l.base, l.one, space, tail)
}

func (l *liveStack) request(cookie, url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if cookie != "" {
		req.Header.Set("Cookie", "taskto_session="+cookie)
	}
	return http.DefaultClient.Do(req)
}

func (l *liveStack) status(cookie, url string) int {
	l.t.Helper()
	res, err := l.request(cookie, url)
	if err != nil {
		l.t.Fatal(err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

type liveStream struct {
	t *testing.T
	r *bufio.Reader
}

func (l *liveStack) tick(i int) {
	l.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		l.mu.Lock()
		have := len(l.rechecks)
		l.mu.Unlock()
		if have > i {
			break
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("stream %d never started its access check clock", i)
		}
		time.Sleep(time.Millisecond)
	}
	l.rechecks[i] <- time.Now()
}

func (l *liveStack) connect(cookie string, space int64) *liveStream {
	l.t.Helper()
	return l.connectTo(cookie, l.path(space, "live"))
}

func (l *liveStack) connectTo(cookie, url string) *liveStream {
	l.t.Helper()
	res, err := l.request(cookie, url)
	if err != nil {
		l.t.Fatal(err)
	}
	l.t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		l.t.Fatalf("connect = %d %s", res.StatusCode, body)
	}
	s := &liveStream{t: l.t, r: bufio.NewReader(res.Body)}
	if ev := s.next(); ev.Type != live.TypeReady {
		l.t.Fatalf("first event = %+v", ev)
	}
	return s
}

func (s *liveStream) next() live.Event {
	s.t.Helper()
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			s.t.Fatalf("reading the stream: %v", err)
		}
		if raw, ok := strings.CutPrefix(line, "data: "); ok {
			var ev live.Event
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				s.t.Fatal(err)
			}
			return ev
		}
	}
}

func (s *liveStream) ended() {
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
		s.t.Fatal("the stream is still open")
	}
}

func (s *liveStream) endedWithoutData() {
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
		s.t.Fatal("the stream is still open")
	}
}

func (l *liveStack) post(cookie string, space int64) int {
	code, _ := l.browser(cookie, http.MethodPost, strings.TrimPrefix(l.path(space, "events"), l.base), map[string]any{})
	return code
}

func TestLiveConnectAndPostCheckTheCallersAccessToTheSpace(t *testing.T) {
	l := newLiveStack(t)
	if code, _ := l.browser(l.alice.cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members", l.one, l.secret),
		map[string]any{"user_id": l.bob.id, "role": "viewer"}); code != http.StatusNoContent {
		t.Fatalf("add a viewer = %d", code)
	}
	for name, c := range map[string]struct {
		cookie string
		space  int64
		want   int
	}{
		"no login":                        {"", l.team, http.StatusUnauthorized},
		"a member of another space":       {l.carol.cookie, l.team, http.StatusNotFound},
		"a viewer":                        {l.bob.cookie, l.secret, http.StatusOK},
		"an editor":                       {l.carol.cookie, l.secret, http.StatusOK},
		"a private space of someone else": {l.bob.cookie, l.alice.private, http.StatusNotFound},
	} {
		if got := l.status(c.cookie, l.path(c.space, "live")); got != c.want {
			t.Errorf("%s: connect = %d, want %d", name, got, c.want)
		}
	}
	if got := l.status(l.alice.cookie, l.path(l.secret, "live")); got != http.StatusNotFound {
		t.Errorf("an admin who is not in the space: connect = %d, want 404", got)
	}

	reader := l.connect(l.carol.cookie, l.secret)
	viewer := l.connect(l.bob.cookie, l.secret)
	if got := l.post(l.bob.cookie, l.secret); got != http.StatusForbidden {
		t.Errorf("a viewer's post = %d, want 403", got)
	}
	if got := l.post(l.alice.cookie, l.secret); got != http.StatusNotFound {
		t.Errorf("a non-member's post = %d, want 404", got)
	}
	if got := l.post(l.bob.cookie, l.team); got != http.StatusNoContent {
		t.Errorf("an editor's post = %d, want 204", got)
	}
	if got := l.post(l.carol.cookie, l.secret); got != http.StatusNoContent {
		t.Fatalf("an editor's post = %d, want 204", got)
	}
	for name, s := range map[string]*liveStream{"editor": reader, "viewer": viewer} {
		if ev := s.next(); ev.Seq != 1 || ev.Type != "stroke" {
			t.Errorf("%s got %+v: only the one allowed post reaches the room", name, ev)
		}
	}
}

func TestARemovedMemberStopsReceivingAtOnceAndOthersKeepTheirStream(t *testing.T) {
	l := newLiveStack(t)
	removed := l.connect(l.bob.cookie, l.team)
	stays := l.connect(l.alice.cookie, l.team)
	other := l.connect(l.carol.cookie, l.secret)

	if code, _ := l.browser(l.bob.cookie, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", l.one, l.team, l.alice.id), nil); code == http.StatusNoContent {
		t.Fatal("a non-admin removed a member")
	}
	if code, body := l.browser(l.alice.cookie, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", l.one, l.team, l.bob.id), nil); code != http.StatusNoContent {
		t.Fatalf("remove = %d %v", code, body)
	}
	removed.ended()
	l.post(l.alice.cookie, l.team)
	if ev := stays.next(); ev.Seq != 1 {
		t.Fatalf("the member who stayed got %+v", ev)
	}
	l.post(l.carol.cookie, l.secret)
	if ev := other.next(); ev.Seq != 1 {
		t.Fatalf("another space's stream got %+v", ev)
	}
	if got := l.status(l.bob.cookie, l.path(l.team, "live")); got != http.StatusNotFound {
		t.Fatalf("the removed member reconnects with %d, want 404", got)
	}
}

func TestAFailedRemovalEndsNothing(t *testing.T) {
	l := newLiveStack(t)
	s := l.connectTo(l.bob.cookie, l.path(l.team, "live")+"?lenient=1")
	l.exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, l.team, l.bob.id)
	if code, _ := l.browser(l.alice.cookie, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", l.one, l.team, l.bob.id), nil); code != http.StatusNotFound {
		t.Fatalf("removing a non-member = %d, want 404", code)
	}
	l.post(l.alice.cookie, l.team)
	if ev := s.next(); ev.Seq != 1 {
		t.Fatalf("got %+v: a refused removal ended a stream", ev)
	}
}

func TestAStreamWhoseAccessWasLostBehindTheHubsBackEndsAtTheNextCheck(t *testing.T) {
	l := newLiveStack(t)
	s := l.connect(l.bob.cookie, l.team)
	l.exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, l.team, l.bob.id)
	l.tick(0)
	s.ended()
}

func TestAStreamThatStillHasAccessSurvivesTheCheck(t *testing.T) {
	l := newLiveStack(t)
	s := l.connect(l.bob.cookie, l.team)
	l.tick(0)
	l.post(l.bob.cookie, l.team)
	if ev := s.next(); ev.Seq != 1 {
		t.Fatalf("got %+v", ev)
	}
}

func (l *liveStack) replaceTeam() {
	l.t.Helper()
	l.exec(`DELETE FROM spaces WHERE id = ?`, l.team)
	l.exec(`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (?, ?, 'shared', 'Team again')`, l.team, l.one)
	l.exec(`INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'editor')`, l.team, l.bob.id)
	if l.scalar(`SELECT generation FROM spaces WHERE id = ?`, l.team) == 0 {
		l.t.Fatal("the new space has no generation")
	}
}

func TestASpaceThatTookTheOldSpacesIDGetsNoneOfItsEvents(t *testing.T) {
	l := newLiveStack(t)
	old := l.connect(l.bob.cookie, l.team)
	l.replaceTeam()
	fresh := l.connect(l.bob.cookie, l.team)
	l.post(l.bob.cookie, l.team)
	if ev := fresh.next(); ev.Seq != 1 {
		t.Fatalf("the new space's stream got %+v", ev)
	}
	l.tick(0)
	old.endedWithoutData()
}

func TestTheHubKeepsTheReplacementSpacesTopicApartFromTheOldOne(t *testing.T) {
	l := newLiveStack(t)
	lenient := func(space int64) string { return l.path(space, "live") + "?lenient=1" }
	oldGeneration := l.scalar(`SELECT generation FROM spaces WHERE id = ?`, l.team)
	old := l.connectTo(l.bob.cookie, lenient(l.team))
	l.replaceTeam()
	fresh := l.connectTo(l.bob.cookie, lenient(l.team))
	l.post(l.bob.cookie, l.team)
	if ev := fresh.next(); ev.Seq != 1 || ev.Type != "stroke" {
		t.Fatalf("the new space's stream got %+v", ev)
	}
	oldTopic := live.Topic{Kind: "room", SpaceID: l.team, Generation: oldGeneration, ResourceID: 7}
	if seq, err := l.hub.Publish(oldTopic, "mark", json.RawMessage(`{}`)); err != nil || seq != 1 {
		t.Fatalf("publishing to the old topic = %d %v, want its own first event", seq, err)
	}
	if ev := old.next(); ev.Type != "mark" || ev.Seq != 1 {
		t.Fatalf("the old stream's first event is %+v: the new space's event reached it", ev)
	}
}

func TestARemovedWorkspaceMemberGetsNoEventBeforeTheNextCheck(t *testing.T) {
	l := newLiveStack(t)
	removed := l.connect(l.bob.cookie, l.team)
	stays := l.connect(l.alice.cookie, l.team)
	if code, body := l.browser(l.alice.cookie, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", l.one, l.bob.id), nil); code != http.StatusNoContent {
		t.Fatalf("remove from the workspace = %d %v", code, body)
	}
	if got := l.post(l.alice.cookie, l.team); got != http.StatusNoContent {
		t.Fatalf("post = %d", got)
	}
	if ev := stays.next(); ev.Seq != 1 {
		t.Fatalf("the member who stayed got %+v", ev)
	}
	removed.endedWithoutData()
}

func TestAWriteWaitingToStartIsRefusedWhenARemovalCommitsBeforeIt(t *testing.T) {
	l := newLiveStack(t)
	removed := l.connect(l.bob.cookie, l.team)
	l.armed.Store(true)
	if got := l.post(l.alice.cookie, l.team); got != http.StatusNoContent {
		t.Fatalf("post = %d", got)
	}
	select {
	case <-l.paused:
	case <-time.After(5 * time.Second):
		t.Fatal("the event never reached the writer")
	}
	if code, body := l.browser(l.alice.cookie, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", l.one, l.bob.id), nil); code != http.StatusNoContent {
		t.Fatalf("remove from the workspace = %d %v", code, body)
	}
	l.letGo()
	removed.endedWithoutData()
}

func TestSigningOutEndsTheStream(t *testing.T) {
	l := newLiveStack(t)
	s := l.connect(l.bob.cookie, l.team)
	if code, body := l.browser(l.bob.cookie, http.MethodPost, "/api/logout", nil); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("logout = %d %v", code, body)
	}
	s.ended()
	if got := l.status(l.bob.cookie, l.path(l.team, "live")); got != http.StatusUnauthorized {
		t.Fatalf("connecting after sign-out = %d, want 401", got)
	}
}

func TestRecheckSpaceAnswersNotFoundForALostSpaceAndNothingElse(t *testing.T) {
	l := newLiveStack(t)
	space, err := l.checker.Check(context.Background(), l.bob.id, l.team)
	if err != nil {
		t.Fatal(err)
	}
	check := live.RecheckSpace(l.checker, l.bob.id, space)
	if err := check(context.Background()); err != nil {
		t.Fatalf("a member's check = %v", err)
	}
	l.exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, l.team, l.bob.id)
	var refused *apierr.Error
	if err := check(context.Background()); !errors.As(err, &refused) || refused.Status != http.StatusNotFound || refused.Code != apierr.CodeSpaceNotFound {
		t.Fatalf("a removed member's check = %v, want a 404 space_not_found", err)
	}
}
