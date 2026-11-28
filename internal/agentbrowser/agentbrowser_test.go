package agentbrowser

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"taskto.ai/taskto/internal/agenttools"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/browser"
	"taskto.ai/taskto/internal/cliaccounts"
	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/drain"
)

type fakeBrowsers struct {
	mu      sync.Mutex
	running map[int64]map[browser.Kind]bool
	actions []struct {
		user int64
		a    browser.Action
	}
	lives []int64
	down  bool
}

func (f *fakeBrowsers) unreachable() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return fmt.Errorf("%w: dial unix: connect: no such file", browser.ErrUnreachable)
	}
	return nil
}

func newFake() *fakeBrowsers { return &fakeBrowsers{running: map[int64]map[browser.Kind]bool{}} }

func (f *fakeBrowsers) Availability(context.Context) map[browser.Kind]bool {
	return map[browser.Kind]bool{browser.Personal: true, browser.Testing: true}
}

func (f *fakeBrowsers) Status(_ context.Context, a cliowner.Account) (map[browser.Kind]browser.State, error) {
	user := a.User
	if err := f.unreachable(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[browser.Kind]browser.State{}
	for _, k := range []browser.Kind{browser.Personal, browser.Testing} {
		st := browser.State{Available: true, Running: f.running[user][k]}
		if st.Running {
			st.Tabs = []browser.Tab{{ID: fmt.Sprintf("T%d", user), Title: fmt.Sprintf("user %d's tab", user)}}
		}
		out[k] = st
	}
	return out, nil
}

func (f *fakeBrowsers) set(user int64, kind browser.Kind, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running[user] == nil {
		f.running[user] = map[browser.Kind]bool{}
	}
	f.running[user][kind] = on
}

func (f *fakeBrowsers) Start(_ context.Context, a cliowner.Account, kind browser.Kind) error {
	user := a.User
	if err := f.unreachable(); err != nil {
		return err
	}
	f.set(user, kind, true)
	return nil
}

func (f *fakeBrowsers) Stop(_ context.Context, a cliowner.Account, kind browser.Kind) error {
	user := a.User
	f.set(user, kind, false)
	return nil
}

func (f *fakeBrowsers) Do(_ context.Context, acct cliowner.Account, a browser.Action) (browser.Result, error) {
	user := acct.User
	if err := f.unreachable(); err != nil {
		return browser.Result{}, err
	}
	f.mu.Lock()
	f.actions = append(f.actions, struct {
		user int64
		a    browser.Action
	}{user, a})
	f.mu.Unlock()
	switch {
	case a.Tab == "gone":
		return browser.Result{}, browser.ErrNoTab
	case strings.Contains(a.URL, "unreachable"):
		return browser.Result{}, errors.New("the page didn't load: net::ERR_NAME_NOT_RESOLVED")
	case strings.Contains(a.URL, "internal"):
		return browser.Result{}, errors.New("the CLI worker is unreachable: dial unix /run/taskto-cli/worker.sock: connect: no such file")
	case a.Op == browser.OpTabs:
		return browser.Result{Tabs: []browser.Tab{{ID: "T1"}}}, nil
	case a.Op == browser.OpSnapshot && a.Tab == "login":
		return browser.Result{Snapshot: "Title: Sign in\n[e1] input text \"Username\" = \"\"\n[e2] input password \"Password\" (empty)"}, nil
	case a.Op == browser.OpSnapshot:
		return browser.Result{Snapshot: "Title: x"}, nil
	}
	return browser.Result{Tab: &browser.Tab{ID: "T1", URL: a.URL, Agent: true}}, nil
}

func (f *fakeBrowsers) Forget(context.Context, cliowner.Account) error { return nil }

func (f *fakeBrowsers) OpenLive(_ context.Context, a cliowner.Account) (io.ReadWriteCloser, error) {
	user := a.User
	f.mu.Lock()
	running := f.running[user][browser.Personal]
	if running {
		f.lives = append(f.lives, user)
	}
	f.mu.Unlock()
	if !running {
		return nil, browser.ErrNotRunning
	}
	ours, theirs := net.Pipe()
	go func() {
		defer theirs.Close()
		_, _ = fmt.Fprintf(theirs, "RFB 003.008\nuser %d\n", user)
		_, _ = io.Copy(theirs, theirs)
	}()
	return ours, nil
}

var setupToken = auth.NewSetupToken()

const password = "fake-password-for-tests"

type env struct {
	ws  int64
	db  *sql.DB
	t   *testing.T
	b   *fakeBrowsers
	svc *Service
	h   http.Handler
}

func newEnv(t *testing.T, b browser.Browsers, usable func(context.Context, int64) error) *env {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: sqlDB, svc: New(b, usable, testAccount)}
	if f, ok := b.(*fakeBrowsers); ok {
		e.b = f
	}
	mux := http.NewServeMux()
	a := auth.New(sqlDB, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken, InsecureCookies: true})
	a.Routes(mux)
	e.svc.Routes(mux, a)
	e.h = auth.CSRF(mux)
	return e
}

type client struct {
	e      *env
	cookie *http.Cookie
	id     int64
}

func (c *client) do(method, path string, body any) *httptest.ResponseRecorder {
	c.e.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		raw, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if c.cookie != nil {
		r.AddCookie(c.cookie)
	}
	rec := httptest.NewRecorder()
	c.e.h.ServeHTTP(rec, r)
	for _, ck := range rec.Result().Cookies() {
		if ck.MaxAge >= 0 {
			c.cookie = ck
		}
	}
	return rec
}

func expect(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

type me struct {
	User struct {
		ID int64 `json:"id"`
	} `json:"user"`
	Workspaces []struct {
		ID int64 `json:"id"`
	} `json:"workspaces"`
}

func (e *env) setup() (owner, member *client) {
	e.t.Helper()
	owner = &client{e: e}
	rec := owner.do(http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": password, "workspace_name": "Home", "setup_token": setupToken,
	})
	expect(e.t, rec, http.StatusCreated)
	m := decode[me](e.t, rec)
	owner.id = m.User.ID
	e.ws = m.Workspaces[0].ID
	rec = owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", m.Workspaces[0].ID), map[string]string{"email": "member@example.com", "role": "member"})
	expect(e.t, rec, http.StatusCreated)
	token := decode[struct {
		Token string `json:"token"`
	}](e.t, rec).Token
	member = &client{e: e}
	rec = member.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token, "name": "Member", "password": password})
	expect(e.t, rec, http.StatusCreated)
	member.id = decode[me](e.t, rec).User.ID
	return owner, member
}

const base = "/api/me/browser"

func TestOff(t *testing.T) {
	e := newEnv(t, nil, nil)
	owner, _ := e.setup()
	st := decode[Status](t, owner.do(http.MethodGet, base, nil))
	if st.Enabled || !strings.Contains(st.Reason, "-browser") || st.ReasonCode != apierr.CodeBrowserOff {
		t.Fatalf("status = %+v", st)
	}
	expect(t, owner.do(http.MethodPost, base+"/start", map[string]string{"browser": "personal"}), http.StatusConflict)
	expect(t, owner.do(http.MethodGet, base+"/live", nil), http.StatusConflict)
	if _, err := e.svc.Call(context.Background(), owner.id, 1, "browser_open", json.RawMessage(`{"url":"https://example.com"}`)); !refusedWith(err, http.StatusConflict, "off") {
		t.Fatalf("a tool with the browser off: %v", err)
	}
}

func TestUnusable(t *testing.T) {
	e := newEnv(t, newFake(), func(context.Context, int64) error { return errors.New("no OS boundary between users") })
	owner, _ := e.setup()
	st := decode[Status](t, owner.do(http.MethodGet, base, nil))
	if st.Enabled || st.Reason != "no OS boundary between users" || st.ReasonCode != apierr.CodeBrowserUnusable {
		t.Fatalf("status = %+v", st)
	}
	rec := owner.do(http.MethodPost, base+"/start", map[string]string{})
	expect(t, rec, http.StatusConflict)
	if code := decode[map[string]string](t, rec)["code"]; code != string(apierr.CodeBrowserUnusable) {
		t.Fatalf("code = %q", code)
	}
	for why, code := range map[error]apierr.Code{
		cliaccounts.ErrOff:        apierr.CodeBrowserCLIWorkersOff,
		cliaccounts.ErrNoBoundary: apierr.CodeBrowserSingleUser,
	} {
		other := newEnv(t, newFake(), func(context.Context, int64) error { return why })
		owner, _ := other.setup()
		if st := decode[Status](t, owner.do(http.MethodGet, base, nil)); st.ReasonCode != code {
			t.Errorf("%v: reason_code = %q, want %q", why, st.ReasonCode, code)
		}
		if len(other.b.actions) != 0 || len(other.b.running) != 0 {
			t.Errorf("%v: the browser was reached", why)
		}
	}
	if len(e.b.actions) != 0 || len(e.b.running) != 0 {
		t.Fatal("the browser was reached")
	}
}

func TestRoutesAreTheCallersOwn(t *testing.T) {
	e := newEnv(t, newFake(), nil)
	owner, member := e.setup()
	expect(t, (&client{e: e}).do(http.MethodGet, base, nil), http.StatusUnauthorized)

	st := decode[Status](t, owner.do(http.MethodPost, base+"/start", map[string]string{"browser": "personal"}))
	if !st.Enabled || !st.Browsers[browser.Personal].Running || st.Browsers[browser.Personal].Tabs[0].ID != fmt.Sprintf("T%d", owner.id) {
		t.Fatalf("the owner's status = %+v", st)
	}
	if st := decode[Status](t, member.do(http.MethodGet, base, nil)); st.Browsers[browser.Personal].Running {
		t.Fatalf("the member sees the owner's browser: %+v", st)
	}
	expect(t, member.do(http.MethodPost, base+"/stop", map[string]string{"browser": "personal"}), http.StatusNoContent)
	if st := decode[Status](t, owner.do(http.MethodGet, base, nil)); !st.Browsers[browser.Personal].Running {
		t.Fatal("the member's stop reached the owner's browser")
	}
	expect(t, member.do(http.MethodPost, base+"/start", map[string]string{"browser": "testing"}), http.StatusOK)
	if !e.b.running[member.id][browser.Testing] || e.b.running[owner.id][browser.Testing] {
		t.Fatalf("running = %v", e.b.running)
	}

	expect(t, owner.do(http.MethodPost, base+"/start", map[string]string{"browser": "other"}), http.StatusBadRequest)
	expect(t, owner.do(http.MethodPost, base+"/start", map[string]any{"browser": "personal", "user": member.id}), http.StatusBadRequest)
	expect(t, owner.do(http.MethodGet, base+"/start", nil), http.StatusMethodNotAllowed)
	r := httptest.NewRequest(http.MethodPost, base+"/stop", strings.NewReader(`{"browser":"personal"}`))
	r.Header.Set("Content-Type", "text/plain")
	r.AddCookie(owner.cookie)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	expect(t, rec, http.StatusUnsupportedMediaType)
}

func liveView(t *testing.T, srv *httptest.Server, c *client, origin string, header http.Header) (*websocket.Conn, error) {
	t.Helper()
	cfg, err := websocket.NewConfig(strings.Replace(srv.URL, "http", "ws", 1)+base+"/live", origin)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Protocol = []string{"binary"}
	cfg.Header = http.Header{}
	for k, v := range header {
		cfg.Header[k] = v
	}
	if c != nil && c.cookie != nil {
		cfg.Header.Set("Cookie", c.cookie.Name+"="+c.cookie.Value)
	}
	return websocket.DialConfig(cfg)
}

func readLine(t *testing.T, r io.Reader) string {
	t.Helper()
	var b []byte
	one := make([]byte, 1)
	for {
		if _, err := r.Read(one); err != nil {
			t.Fatalf("reading: %v (so far %q)", err, b)
		}
		if one[0] == '\n' {
			return string(b)
		}
		b = append(b, one[0])
	}
}

func TestLiveView(t *testing.T) {
	e := newEnv(t, newFake(), nil)
	owner, member := e.setup()
	srv := httptest.NewServer(e.h)
	defer srv.Close()

	if _, err := liveView(t, srv, owner, srv.URL, nil); err == nil {
		t.Fatal("a live view of no browser")
	}
	expect(t, owner.do(http.MethodPost, base+"/start", map[string]string{}), http.StatusOK)

	ws, err := liveView(t, srv, owner, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Config().Protocol[0] != "binary" {
		t.Errorf("protocol = %v", ws.Config().Protocol)
	}
	if got := readLine(t, ws); got != "RFB 003.008" {
		t.Fatalf("banner %q", got)
	}
	if got := readLine(t, ws); got != fmt.Sprintf("user %d", owner.id) {
		t.Fatalf("the live view is %q", got)
	}
	if _, err := ws.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, ws); got != "ping" {
		t.Fatalf("echo %q", got)
	}
	ws.Close()

	if _, err := liveView(t, srv, owner, "https://evil.example", nil); err == nil {
		t.Error("a cross-origin live view was opened")
	}
	if _, err := liveView(t, srv, owner, srv.URL, http.Header{"Sec-Fetch-Site": {"cross-site"}}); err == nil {
		t.Error("a cross-site live view was opened")
	}
	if _, err := liveView(t, srv, nil, srv.URL, nil); err == nil {
		t.Error("a live view without a session")
	}
	if _, err := liveView(t, srv, member, srv.URL, nil); err == nil {
		t.Error("the member watched a browser that isn't theirs")
	}
	expect(t, member.do(http.MethodPost, base+"/start", map[string]string{}), http.StatusOK)
	ws, err = liveView(t, srv, member, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	readLine(t, ws)
	if got := readLine(t, ws); got != fmt.Sprintf("user %d", member.id) {
		t.Fatalf("the member's live view is %q", got)
	}
	ws.Close()
	for _, u := range e.b.lives {
		if u != owner.id && u != member.id {
			t.Errorf("a live view for user %d", u)
		}
	}
	if len(e.b.lives) != 2 {
		t.Errorf("live views opened: %v", e.b.lives)
	}
}

func liveEnds(ws *websocket.Conn) bool {
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	for {
		if _, err := ws.Read(buf); err != nil {
			var ne net.Error
			return !errors.As(err, &ne) || !ne.Timeout()
		}
	}
}

func openLive(t *testing.T, srv *httptest.Server, c *client) *websocket.Conn {
	t.Helper()
	expect(t, c.do(http.MethodPost, base+"/start", map[string]string{}), http.StatusOK)
	ws, err := liveView(t, srv, c, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	readLine(t, ws)
	readLine(t, ws)
	return ws
}

func TestLiveViewEndsWithTheLogin(t *testing.T) {
	old := liveRecheck
	t.Cleanup(func() { liveRecheck = old })

	t.Run("signed out", func(t *testing.T) {
		liveRecheck = time.Hour
		e := newEnv(t, newFake(), nil)
		owner, member := e.setup()
		srv := httptest.NewServer(e.h)
		defer srv.Close()
		ws := openLive(t, srv, owner)
		other := openLive(t, srv, member)
		expect(t, owner.do(http.MethodPost, "/api/logout", nil), http.StatusNoContent)
		if !liveEnds(ws) {
			t.Fatal("the live view outlived its login")
		}
		if _, err := other.Write([]byte("ping\n")); err != nil {
			t.Fatal(err)
		}
		if got := readLine(t, other); got != "ping" {
			t.Fatalf("echo %q", got)
		}
	})
	t.Run("revoked behind the signal", func(t *testing.T) {
		liveRecheck = 20 * time.Millisecond
		e := newEnv(t, newFake(), nil)
		owner, _ := e.setup()
		srv := httptest.NewServer(e.h)
		defer srv.Close()
		ws := openLive(t, srv, owner)
		if _, err := e.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, owner.id); err != nil {
			t.Fatal(err)
		}
		if !liveEnds(ws) {
			t.Fatal("the live view outlived its login")
		}
	})
}

func TestLiveViewEndsWhenTheMemberIsDisabled(t *testing.T) {
	old := liveRecheck
	liveRecheck = time.Hour
	t.Cleanup(func() { liveRecheck = old })
	e := newEnv(t, newFake(), nil)
	owner, member := e.setup()
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	ws := openLive(t, srv, member)
	defer ws.Close()
	other := openLive(t, srv, owner)
	defer other.Close()

	expect(t, owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/disable", e.ws, member.id), nil), http.StatusNoContent)
	if !liveEnds(ws) {
		t.Fatal("a disabled member's live view kept running")
	}
	if _, err := other.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, other); got != "ping" {
		t.Fatalf("the owner's view: %q", got)
	}
	if _, err := liveView(t, srv, member, srv.URL, nil); err == nil {
		t.Fatal("a disabled member opened a live view")
	}
}

func TestSameOrigin(t *testing.T) {
	for _, c := range []struct {
		host, site, origin string
		ok                 bool
	}{
		{"tasks.example.com", "same-origin", "", true},
		{"tasks.example.com", "same-site", "https://tasks.example.com", false},
		{"tasks.example.com", "cross-site", "https://tasks.example.com", false},
		{"tasks.example.com", "", "https://tasks.example.com", true},
		{"tasks.example.com", "", "https://Tasks.Example.com", true},
		{"tasks.example.com", "", "https://evil.example", false},
		{"tasks.example.com", "", "https://tasks.example.com.evil.example", false},
		{"tasks.example.com", "", "null", false},
		{"tasks.example.com", "", "", false},
		{"tasks.example.com:443", "", "https://tasks.example.com", true},
		{"tasks.example.com", "", "https://tasks.example.com:443", true},
		{"localhost:80", "", "http://localhost", true},
		{"tasks.example.com:8443", "", "https://tasks.example.com", false},
		{"tasks.example.com:443", "", "http://tasks.example.com", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/me/browser/live", nil)
		r.Host = c.host
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if got := sameOrigin(r); got != c.ok {
			t.Errorf("%+v: %v", c, got)
		}
	}
}

func refusedWith(err error, status int, text string) bool {
	var r *agenttools.Refusal
	return errors.As(err, &r) && r.Status == status && strings.Contains(r.Message, text)
}

func TestTools(t *testing.T) {
	f := newFake()
	s := New(f, nil, testAccount)
	ctx := context.Background()
	call := func(name, args string) (map[string]any, error) {
		out, err := s.Call(ctx, 7, 3, name, json.RawMessage(args))
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(out)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m, nil
	}
	if _, err := call("browser_open", `{"url":"https://example.com"}`); err != nil {
		t.Fatal(err)
	}
	last := f.actions[len(f.actions)-1]
	if last.user != 7 || last.a.Op != browser.OpOpen || last.a.Browser != browser.Personal || last.a.URL != "https://example.com" {
		t.Fatalf("open reached %+v", last)
	}
	if _, err := call("browser_type", `{"tab":"T1","ref":"e2","text":"hello","submit":true,"browser":"testing"}`); err != nil {
		t.Fatal(err)
	}
	if a := f.actions[len(f.actions)-1].a; a.Op != browser.OpType || a.Text != "hello" || !a.Submit || a.Browser != browser.Testing {
		t.Fatalf("type = %+v", a)
	}
	out, err := call("browser_help", `{"tab":"T1","reason":"login","message":"Sign in to your bank, then press Done."}`)
	if err != nil || out["status"] != "waiting_for_user" || !strings.Contains(out["note"].(string), "End your turn") {
		t.Fatalf("help = %v, %v", out, err)
	}
	if a := f.actions[len(f.actions)-1].a; a.Op != browser.OpFront || a.Browser != browser.Personal {
		t.Fatalf("help brought %+v to the front", a)
	}

	if out, err := call("browser_snapshot", `{"tab":"login"}`); err != nil || !strings.Contains(fmt.Sprint(out["note"]), "call browser_help") {
		t.Errorf("a sign-in page = %v, %v", out, err)
	}
	if out, err := call("browser_snapshot", `{"tab":"login","browser":"testing"}`); err != nil || out["note"] != nil {
		t.Errorf("a sign-in page in the testing browser = %v, %v", out, err)
	}
	if out, err := call("browser_snapshot", `{"tab":"T1"}`); err != nil || out["note"] != nil {
		t.Errorf("a page without a password field = %v, %v", out, err)
	}

	for _, c := range []struct{ name, args, want string }{
		{"browser_open", `{"url":"https://example.com","user":2}`, `doesn't take "user"`},
		{"browser_open", `{"url":"https://example.com"} {}`, "one JSON object"},
		{"browser_type", `{"tab":"T1","ref":"e2"}`, "text is needed"},
		{"browser_help", `{"tab":"T1","reason":"bored","message":"x"}`, "reason must be"},
		{"browser_help", `{"tab":"T1","reason":"login","message":"  "}`, "message must"},
		{"browser_help", `{"tab":"T1","reason":"login","message":"x","browser":"testing"}`, `doesn't take "browser"`},
		{"browser_open", `{"url":"file:///etc/passwd"}`, "http or https"},
		{"browser_open", `{"url":"javascript:alert(1)"}`, "http or https"},
		{"browser_navigate", `{"tab":"T1","url":"chrome://settings"}`, "http or https"},
		{"browser_tabs", `{"browser":"other"}`, "browser must be"},
		{"browser_snapshot", `{"tab":"../x"}`, "tab id"},
		{"browser_click", `{"tab":"T1","ref":"12"}`, "ref must be"},
		{"browser_press", `{"tab":"T1","key":"F13"}`, "key must be"},
	} {
		if _, err := call(c.name, c.args); !refusedWith(err, http.StatusBadRequest, c.want) {
			t.Errorf("%s %s: %v", c.name, c.args, err)
		}
	}
	before := len(f.actions)
	if _, err := call("browser_help", `{"tab":"T1","reason":"other","message":"`+strings.Repeat("é", 500)+`"}`); err != nil {
		t.Errorf("500 characters: %v", err)
	}
	if _, err := call("browser_help", `{"tab":"T1","reason":"other","message":"`+strings.Repeat("é", 501)+`"}`); !refusedWith(err, http.StatusBadRequest, "message must") {
		t.Errorf("501 characters: %v", err)
	}
	if len(f.actions) != before+1 {
		t.Errorf("refused calls reached the browser: %d actions", len(f.actions)-before)
	}
	if _, err := call("browser_snapshot", `{"tab":"gone"}`); !refusedWith(err, http.StatusNotFound, "no such tab") {
		t.Errorf("a gone tab: %v", err)
	}
	if _, err := call("browser_open", `{"url":"https://unreachable.example"}`); !refusedWith(err, http.StatusBadGateway, "ERR_NAME_NOT_RESOLVED") {
		t.Errorf("an unreachable page: %v", err)
	}
	if _, err := call("browser_open", `{"url":"https://internal.example"}`); !refusedWith(err, http.StatusBadGateway, "the browser failed") ||
		strings.Contains(err.Error(), "/run/") || strings.Contains(err.Error(), "worker") {
		t.Errorf("an internal error: %v", err)
	}
	if _, err := call("browser_eval", `{}`); !refusedWith(err, http.StatusNotFound, "unknown tool") {
		t.Errorf("an unknown tool: %v", err)
	}
	for _, a := range f.actions {
		if a.user != 7 {
			t.Errorf("an action for user %d", a.user)
		}
	}
}

func TestLiveViewEndsWithTheShutdown(t *testing.T) {
	old := liveRecheck
	t.Cleanup(func() { liveRecheck = old })
	liveRecheck = time.Hour

	e := newEnv(t, newFake(), nil)
	owner, _ := e.setup()
	tracker := drain.New()
	srv := httptest.NewUnstartedServer(tracker.Handler(e.h))
	srv.Start()
	defer srv.Close()
	ws := openLive(t, srv, owner)
	defer ws.Close()

	began := time.Now()
	if err := tracker.Shutdown(srv.Config, 0, 3*time.Second); err != nil {
		t.Fatalf("shutdown with a live view open: %v", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("shutdown took %s with a live view open", took)
	}
	if !liveEnds(ws) {
		t.Fatal("the live view outlived the shutdown")
	}
}

func TestNotReadyKeepsItsCode(t *testing.T) {
	notReady := apierr.New(http.StatusServiceUnavailable, apierr.CodeCLIWorkerNotReady, "the CLI worker isn't ready yet: try again in a moment")
	var asked []int64
	e := newEnv(t, newFake(), func(_ context.Context, user int64) error {
		asked = append(asked, user)
		return notReady
	})
	owner, _ := e.setup()
	for _, r := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, base, nil},
		{http.MethodPost, base + "/start", map[string]string{}},
		{http.MethodPost, base + "/stop", map[string]string{}},
	} {
		rec := owner.do(r.method, r.path, r.body)
		if rec.Code != http.StatusServiceUnavailable || decode[map[string]any](t, rec)["code"] != string(apierr.CodeCLIWorkerNotReady) {
			t.Errorf("%s %s = %d %s, want 503 %s", r.method, r.path, rec.Code, rec.Body.String(), apierr.CodeCLIWorkerNotReady)
		}
	}
	for _, user := range asked {
		if user != owner.id {
			t.Errorf("readiness was asked for user %d, not the caller %d", user, owner.id)
		}
	}
	if _, err := New(e.b, func(context.Context, int64) error { return notReady }, testAccount).Call(context.Background(), owner.id, 1, "browser_tabs", json.RawMessage(`{}`)); !refusedWith(err, http.StatusServiceUnavailable, "isn't ready") {
		t.Errorf("a tool while the worker isn't ready: %v", err)
	}
	if len(e.b.actions) != 0 || len(e.b.running) != 0 {
		t.Fatal("the browser was reached")
	}
}

func TestAnUnreachableWorkerIsNotReady(t *testing.T) {
	f := newFake()
	e := newEnv(t, f, nil)
	owner, _ := e.setup()
	f.down = true
	for _, r := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, base, nil},
		{http.MethodPost, base + "/start", map[string]string{}},
	} {
		rec := owner.do(r.method, r.path, r.body)
		if rec.Code != http.StatusServiceUnavailable || decode[map[string]any](t, rec)["code"] != string(apierr.CodeCLIWorkerNotReady) {
			t.Errorf("%s %s = %d %s, want 503 %s", r.method, r.path, rec.Code, rec.Body.String(), apierr.CodeCLIWorkerNotReady)
		}
	}
	_, err := New(f, nil, testAccount).Call(context.Background(), owner.id, 1, "browser_tabs", json.RawMessage(`{}`))
	var refused *agenttools.Refusal
	if !errors.As(err, &refused) || refused.Status != http.StatusServiceUnavailable || refused.Code != apierr.CodeCLIWorkerNotReady {
		t.Errorf("a tool with the worker unreachable: %#v", err)
	}
	if _, err := New(e.b, func(context.Context, int64) error {
		return apierr.New(http.StatusServiceUnavailable, apierr.CodeCLIWorkerNotReady, "not ready")
	}, testAccount).Call(context.Background(), owner.id, 1, "browser_tabs", json.RawMessage(`{}`)); !errors.As(err, &refused) || refused.Code != apierr.CodeCLIWorkerNotReady {
		t.Errorf("a tool before the worker is ready keeps no code: %#v", err)
	}
}

func testAccount(_ context.Context, user int64) (cliowner.Account, error) {
	return cliowner.Account{User: user, Identity: fmt.Sprintf("%032x", user)}, nil
}
