package auth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/db"
)

// Test passwords: obviously fake, and long enough for checkPassword.
const (
	ownerPassword = "fake-owner-password"
	otherPassword = "fake-other-password"
)

// testSetupToken is the setup token newEnv gives a server that has none set.
var testSetupToken = NewSetupToken()

// cheap keeps argon2id fast in tests, the race detector included.
var cheap = Params{Memory: 64, Time: 1, Threads: 1}

type env struct {
	t   *testing.T
	db  *sql.DB
	svc *Service
	h   http.Handler

	mu  sync.Mutex
	now time.Time
}

func newEnv(t *testing.T, opts Options) *env {
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
	if opts.HashParams == (Params{}) {
		opts.HashParams = cheap
	}
	if opts.SetupToken == "" {
		opts.SetupToken = testSetupToken
	}
	e := &env{t: t, db: sqlDB, svc: New(sqlDB, opts), now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	e.svc.now = func() time.Time {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.now
	}
	mux := http.NewServeMux()
	e.svc.Routes(mux)
	e.h = e.svc.ClientAddr(CSRF(mux))
	return e
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

// client is a browser: it keeps the session cookie and sends same-origin requests.
type client struct {
	e      *env
	ip     string
	xff    string // X-Forwarded-For, if not empty
	cookie *http.Cookie
}

func (e *env) client() *client { return &client{e: e, ip: "192.0.2.1:1234"} }

// req builds a same-origin request; a non-nil body is sent as JSON.
func (c *client) req(method, path string, body any) *http.Request {
	c.e.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			c.e.t.Fatal(err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
	}
	if method != http.MethodGet {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.RemoteAddr = c.ip
	if c.xff != "" {
		r.Header.Set("X-Forwarded-For", c.xff)
	}
	if c.cookie != nil {
		r.AddCookie(c.cookie)
	}
	return r
}

func (c *client) send(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	c.e.h.ServeHTTP(rec, r)
	for _, ck := range rec.Result().Cookies() {
		if ck.MaxAge < 0 {
			c.cookie = nil
		} else {
			c.cookie = ck
		}
	}
	return rec
}

func (c *client) do(method, path string, body any) *httptest.ResponseRecorder {
	c.e.t.Helper()
	return c.send(c.req(method, path, body))
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

// setup runs first-run setup and returns the owner's client and workspace ID.
func (e *env) setup() (*client, int64) {
	e.t.Helper()
	owner := e.client()
	rec := owner.do(http.MethodPost, "/api/setup", map[string]string{
		"email": "Owner@Example.com", "name": "Owner", "password": ownerPassword, "workspace_name": "Home",
		"setup_token": testSetupToken,
	})
	expect(e.t, rec, http.StatusCreated)
	return owner, decode[me](e.t, rec).Workspaces[0].WorkspaceID
}

// invite has admin invite email to workspace and returns the token.
func (e *env) invite(admin *client, workspace int64, email string, role Role) string {
	e.t.Helper()
	rec := admin.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", workspace),
		map[string]string{"email": email, "role": string(role)})
	expect(e.t, rec, http.StatusCreated)
	return decode[struct{ Token string }](e.t, rec).Token
}

// join accepts an invite as a new person and returns their logged-in client.
func (e *env) join(token, name string) (*client, me) {
	e.t.Helper()
	c := e.client()
	rec := c.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token, "name": name, "password": otherPassword})
	expect(e.t, rec, http.StatusCreated)
	return c, decode[me](e.t, rec)
}

// addWorkspace makes a second workspace with userID as its admin, as a later card's
// "create workspace" would.
func (e *env) addWorkspace(userID int64, name string) int64 {
	e.t.Helper()
	res, err := e.db.Exec(`INSERT INTO workspaces (name) VALUES (?)`, name)
	if err != nil {
		e.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, id, userID); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestSetup(t *testing.T) {
	e := newEnv(t, Options{})
	anon := e.client()
	if got := decode[map[string]bool](t, anon.do(http.MethodGet, "/api/setup", nil)); !got["needed"] {
		t.Fatalf("GET /api/setup = %v before setup, want needed", got)
	}

	owner, ws := e.setup()
	ck := owner.cookie
	if ck == nil || ck.Name != "__Host-taskto_session" || !ck.Secure || !ck.HttpOnly ||
		ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" || ck.Domain != "" || ck.MaxAge != int((30*24*time.Hour).Seconds()) {
		t.Fatalf("session cookie = %+v", ck)
	}

	got := decode[me](t, owner.do(http.MethodGet, "/api/me", nil))
	want := me{User: meUser{User: User{ID: got.User.ID, Email: "owner@example.com", Name: "Owner"}},
		Workspaces:         []Membership{{WorkspaceID: ws, WorkspaceName: "Home", Role: RoleAdmin}},
		CanCreateWorkspace: true} // the owner administers a workspace, and the default lets admins make more
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("GET /api/me = %+v, want %+v", got, want)
	}

	if got := decode[map[string]bool](t, anon.do(http.MethodGet, "/api/setup", nil)); got["needed"] {
		t.Fatal("GET /api/setup says needed after setup")
	}
	rec := anon.do(http.MethodPost, "/api/setup", map[string]string{
		"email": "late@example.com", "name": "Late", "password": otherPassword, "workspace_name": "Mine",
		"setup_token": testSetupToken,
	})
	expect(t, rec, http.StatusConflict)
	if anon.cookie != nil {
		t.Fatal("a refused setup set a session cookie")
	}
	if n := e.count(`SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("users = %d after a refused setup, want 1", n)
	}
}

func TestSetupValidation(t *testing.T) {
	e := newEnv(t, Options{})
	valid := map[string]any{"email": "a@example.com", "name": "A", "password": ownerPassword, "workspace_name": "W", "setup_token": testSetupToken}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for k, v := range valid {
			m[k] = v
		}
		m[k] = v
		return m
	}
	for name, body := range map[string]any{
		"bad email":          with("email", "not an address"),
		"display name email": with("email", "A <a@example.com>"),
		"short password":     with("password", "short"),
		"no name":            with("name", "  "),
		"no workspace":       with("workspace_name", ""),
		"unknown field":      with("admin", true),
		"wrong type":         with("email", 7),
	} {
		t.Run(name, func(t *testing.T) {
			expect(t, e.client().do(http.MethodPost, "/api/setup", body), http.StatusBadRequest)
		})
	}
	if n := e.count(`SELECT count(*) FROM users`); n != 0 {
		t.Fatalf("users = %d after invalid setups", n)
	}
}

// TestConcurrentSetup races several setups: exactly one may create the owner.
func TestConcurrentSetup(t *testing.T) {
	e := newEnv(t, Options{})
	const n = 12
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			rec := httptest.NewRecorder()
			e.h.ServeHTTP(rec, e.client().req(http.MethodPost, "/api/setup", map[string]string{
				"email": fmt.Sprintf("owner%d@example.com", i), "name": "Owner", "password": ownerPassword, "workspace_name": "Home",
				"setup_token": testSetupToken,
			}))
			codes[i] = rec.Code
		})
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Fatalf("a setup answered %d, want 201 or 409 (all: %v)", c, codes)
		}
	}
	if created != 1 {
		t.Fatalf("%d setups succeeded, want 1 (all: %v)", created, codes)
	}
	for table, want := range map[string]int{"users": 1, "workspaces": 1, "memberships": 1, "sessions": 1} {
		if got := e.count(`SELECT count(*) FROM ` + table); got != want {
			t.Errorf("%s = %d, want %d", table, got, want)
		}
	}
}

// TestErrorsHaveCodes checks that errors carry their stable code and params, which the web
// app translates (internal/apierr).
func TestErrorsHaveCodes(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	type body struct {
		Error  string         `json:"error"`
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	check := func(rec *httptest.ResponseRecorder, status int, code string, params map[string]any) {
		t.Helper()
		expect(t, rec, status)
		got := decode[body](t, rec)
		if got.Code != code || got.Error == "" || fmt.Sprint(got.Params) != fmt.Sprint(params) {
			t.Fatalf("got %+v, want code %q and params %v", got, code, params)
		}
	}
	check(e.client().do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": "not it at all"}),
		http.StatusUnauthorized, "wrong_credentials", nil)
	check(e.client().do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized, "not_logged_in", nil)
	check(owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/tokens", ws), map[string]string{"name": ""}),
		http.StatusBadRequest, "name_length", map[string]any{"max": float64(maxNameLen)})
	check(owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", ws), map[string]string{"email": "nope"}),
		http.StatusBadRequest, "email_invalid", nil)
	check(owner.do(http.MethodPatch, "/api/me", map[string]string{"locale": "pt_BR"}), http.StatusBadRequest, "locale_invalid", nil)
	check(owner.do(http.MethodPatch, "/api/me", "not an object"), http.StatusBadRequest, "invalid_json", nil)
	check(owner.do(http.MethodGet, "/api/workspaces/999/members", nil), http.StatusNotFound, "workspace_not_found", nil)
	check(owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/x", ws), nil), http.StatusNotFound, "member_not_found", nil)
	check(owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, decode[me](t, owner.do(http.MethodGet, "/api/me", nil)).User.ID), nil),
		http.StatusConflict, "last_admin", nil)
}

func TestLoginAndLogout(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	setupCookie := owner.cookie

	c := e.client()
	expect(t, c.do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": "fake-wrong-password"}), http.StatusUnauthorized)
	expect(t, c.do(http.MethodPost, "/api/login", map[string]string{"email": "nobody@example.com", "password": ownerPassword}), http.StatusUnauthorized)
	expect(t, c.do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com"}), http.StatusBadRequest)
	if c.cookie != nil {
		t.Fatal("a failed login set a cookie")
	}

	// The address is matched without regard to case or surrounding space.
	rec := c.do(http.MethodPost, "/api/login", map[string]string{"email": " OWNER@example.com ", "password": ownerPassword})
	expect(t, rec, http.StatusOK)
	if got := decode[me](t, rec); got.User.Email != "owner@example.com" || len(got.Workspaces) != 1 {
		t.Fatalf("login reply = %+v", got)
	}
	expect(t, c.do(http.MethodGet, "/api/me", nil), http.StatusOK)

	// Logging in again replaces the browser's previous session.
	before := c.cookie
	expect(t, c.do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": ownerPassword}), http.StatusOK)
	stale := &client{e: e, ip: c.ip, cookie: before}
	expect(t, stale.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)

	current := c.cookie
	expect(t, c.do(http.MethodPost, "/api/logout", nil), http.StatusNoContent)
	if c.cookie != nil {
		t.Fatal("logout didn't clear the cookie")
	}
	// The revoked session doesn't work even if the browser kept the cookie.
	revoked := &client{e: e, ip: c.ip, cookie: current}
	expect(t, revoked.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)

	// Other sessions of the same user are untouched.
	other := &client{e: e, ip: c.ip, cookie: setupCookie}
	expect(t, other.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	// Logging out without a session is fine.
	expect(t, e.client().do(http.MethodPost, "/api/logout", nil), http.StatusNoContent)
}

func TestSessionsAreStoredHashedAndExpire(t *testing.T) {
	e := newEnv(t, Options{SessionTTL: time.Hour})
	owner, _ := e.setup()
	token := owner.cookie.Value
	if n := e.count(`SELECT count(*) FROM sessions WHERE token_hash = ?`, token); n != 0 {
		t.Fatal("the session token is stored in clear")
	}
	if n := e.count(`SELECT count(*) FROM sessions WHERE token_hash = ?`, hashToken(token)); n != 1 {
		t.Fatal("the session's hash isn't stored")
	}

	e.advance(59 * time.Minute)
	expect(t, owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	e.advance(time.Minute)
	expect(t, owner.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	if owner.cookie != nil {
		t.Fatal("an expired session's cookie wasn't cleared")
	}

	// The next login drops the user's expired sessions.
	c := e.client()
	expect(t, c.do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": ownerPassword}), http.StatusOK)
	if n := e.count(`SELECT count(*) FROM sessions`); n != 1 {
		t.Fatalf("sessions = %d after logging in again, want only the new one", n)
	}
}

func TestBadSessionCookie(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	c := e.client()
	c.cookie = &http.Cookie{Name: "__Host-taskto_session", Value: "made-up-token"} // #nosec G124 -- a request cookie in a test
	expect(t, c.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
}

func TestInsecureCookies(t *testing.T) {
	e := newEnv(t, Options{InsecureCookies: true})
	owner, _ := e.setup()
	if ck := owner.cookie; ck.Name != "taskto_session" || ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie = %+v, want taskto_session without Secure", ck)
	}
	expect(t, owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)
}

func TestLoginRateLimitPerAccount(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	login := func(c *client, password string) *httptest.ResponseRecorder {
		return c.do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": password})
	}
	// Successful logins don't count.
	for range accountAttempts + 1 {
		expect(t, login(e.client(), ownerPassword), http.StatusOK)
	}
	// Failures from many addresses add up for the account.
	for i := range accountAttempts {
		c := e.client()
		c.ip = fmt.Sprintf("198.51.100.%d:1", i)
		expect(t, login(c, "fake-wrong-password"), http.StatusUnauthorized)
	}
	rec := login(e.client(), ownerPassword)
	expect(t, rec, http.StatusTooManyRequests)
	if ra := rec.Header().Get("Retry-After"); ra != "900" {
		t.Fatalf("Retry-After = %q, want 900", ra)
	}
	e.advance(attemptWindow)
	expect(t, login(e.client(), ownerPassword), http.StatusOK)
}

func TestLoginRateLimitPerIP(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	attacker := e.client()
	attacker.ip = "203.0.113.9:4444"
	for i := range ipAttempts {
		// Different (unknown) accounts: only the address limit applies.
		rec := attacker.do(http.MethodPost, "/api/login", map[string]string{"email": fmt.Sprintf("guess%d@example.com", i), "password": otherPassword})
		expect(t, rec, http.StatusUnauthorized)
	}
	expect(t, attacker.do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": ownerPassword}), http.StatusTooManyRequests)
	// Someone else isn't affected.
	expect(t, e.client().do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": ownerPassword}), http.StatusOK)
	// Unknown addresses are tracked like real ones (so answers don't differ), but the
	// address limit comes first, so one client adds at most ipAttempts keys per window.
	if n := len(e.svc.perAccount.hits); n > ipAttempts+1 {
		t.Fatalf("per-account limiter tracks %d keys, want at most %d", n, ipAttempts+1)
	}
}

// Behind a trusted proxy each client has its own address limit, and rewriting
// X-Forwarded-For doesn't get a client a fresh one.
func TestLoginRateLimitBehindProxy(t *testing.T) {
	e := newEnv(t, Options{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}})
	e.setup()
	viaProxy := func(xff string) *client {
		c := e.client()
		c.ip, c.xff = "10.0.0.2:5000", xff
		return c
	}
	guess := func(c *client, i int) *httptest.ResponseRecorder {
		return c.do(http.MethodPost, "/api/login", map[string]string{"email": fmt.Sprintf("guess%d@example.com", i), "password": otherPassword})
	}
	for i := range ipAttempts {
		// The attacker makes up a new left-most address every time; the proxy appends the real one.
		expect(t, guess(viaProxy(fmt.Sprintf("192.0.2.%d, 203.0.113.9", i)), i), http.StatusUnauthorized)
	}
	expect(t, guess(viaProxy("198.51.100.1, 203.0.113.9"), 0), http.StatusTooManyRequests)
	// Another client behind the same proxy isn't affected.
	expect(t, viaProxy("198.51.100.20").do(http.MethodPost, "/api/login",
		map[string]string{"email": "owner@example.com", "password": ownerPassword}), http.StatusOK)

	// Reaching the server directly, the attacker can't pose as someone else either: an
	// untrusted peer's header is ignored.
	direct := e.client()
	direct.ip = "203.0.113.50:1"
	for i := range ipAttempts {
		direct.xff = fmt.Sprintf("198.51.100.%d", 100+i)
		expect(t, guess(direct, i), http.StatusUnauthorized)
	}
	direct.xff = "198.51.100.250"
	expect(t, guess(direct, 0), http.StatusTooManyRequests)
}

// An attacker's failures neither reveal which emails have accounts nor lock the real
// owner out from another address.
func TestLoginLimitsDontRevealOrLockAccounts(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	attacker := e.client()
	attacker.ip = "203.0.113.7:1"
	for _, email := range []string{"owner@example.com", "nobody@example.com"} {
		for range accountAndIPAttempts {
			expect(t, attacker.do(http.MethodPost, "/api/login", map[string]string{"email": email, "password": "fake-wrong-password"}), http.StatusUnauthorized)
		}
		// The same answer whether the account exists or not.
		expect(t, attacker.do(http.MethodPost, "/api/login", map[string]string{"email": email, "password": "fake-wrong-password"}), http.StatusTooManyRequests)
	}
	expect(t, e.client().do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": ownerPassword}), http.StatusOK)
}

func TestCSRF(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	invites := fmt.Sprintf("/api/workspaces/%d/invites", ws)
	body := map[string]string{"email": "new@example.com"}

	tests := []struct {
		name   string
		edit   func(*http.Request)
		status int
	}{
		{"cross-site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, http.StatusForbidden},
		{"same-site, other origin", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }, http.StatusForbidden},
		{"foreign Origin, no Sec-Fetch-Site", func(r *http.Request) {
			r.Header.Del("Sec-Fetch-Site")
			r.Header.Set("Origin", "https://evil.example")
		}, http.StatusForbidden},
		{"form post", func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, http.StatusUnsupportedMediaType},
		{"text/plain", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType},
		{"no content type", func(r *http.Request) { r.Header.Del("Content-Type") }, http.StatusUnsupportedMediaType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := owner.req(http.MethodPost, invites, body)
			tt.edit(r)
			expect(t, owner.send(r), tt.status)
		})
	}
	if n := e.count(`SELECT count(*) FROM invites`); n != 0 {
		t.Fatalf("invites = %d after refused requests, want 0", n)
	}

	// A cross-site logout or login is refused too, so a page elsewhere can't sign the
	// user out or into another account; that holds without a body or content type.
	r := owner.req(http.MethodPost, "/api/logout", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Del("Content-Type")
	expect(t, owner.send(r), http.StatusForbidden)
	expect(t, owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)

	// What passes: same-origin JSON, a matching Origin, and a client sending neither
	// header (curl, scripts), which carries no ambient browser cookie.
	r = owner.req(http.MethodPost, invites, body)
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	expect(t, owner.send(r), http.StatusCreated)
	r = owner.req(http.MethodPost, invites, map[string]string{"email": "two@example.com"})
	r.Header.Del("Sec-Fetch-Site")
	r.Header.Set("Origin", "http://example.com") // httptest's Host
	expect(t, owner.send(r), http.StatusCreated)
	r = owner.req(http.MethodPost, invites, map[string]string{"email": "three@example.com"})
	r.Header.Del("Sec-Fetch-Site")
	expect(t, owner.send(r), http.StatusCreated)

	// Safe methods aren't checked: they never change state.
	r = owner.req(http.MethodGet, "/api/me", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	expect(t, owner.send(r), http.StatusOK)
}

func TestInviteNewPerson(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	token := e.invite(owner, ws, "New@Example.com", RoleMember)
	if len(token) != 43 {
		t.Fatalf("token %q, want 43 characters of base64url", token)
	}
	if n := e.count(`SELECT count(*) FROM invites WHERE token_hash = ?`, hashToken(token)); n != 1 {
		t.Fatal("the invite's hash isn't stored")
	}

	// Listed while pending, without the token.
	rec := owner.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/invites", ws), nil)
	expect(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), token) || !strings.Contains(rec.Body.String(), "new@example.com") {
		t.Fatalf("invites = %s", rec.Body.String())
	}

	// Invalid input doesn't use the invite.
	anon := e.client()
	expect(t, anon.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token, "name": "New", "password": "short"}), http.StatusBadRequest)
	expect(t, anon.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token, "name": "", "password": otherPassword}), http.StatusBadRequest)
	expect(t, anon.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": "made-up-token", "name": "New", "password": otherPassword}), http.StatusNotFound)

	newcomer, got := e.join(token, "New Person")
	if got.User.Email != "new@example.com" || got.User.Name != "New Person" ||
		len(got.Workspaces) != 1 || got.Workspaces[0].WorkspaceID != ws || got.Workspaces[0].Role != RoleMember {
		t.Fatalf("accept reply = %+v", got)
	}
	expect(t, newcomer.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	// And they can log in with the password they chose.
	expect(t, e.client().do(http.MethodPost, "/api/login", map[string]string{"email": "new@example.com", "password": otherPassword}), http.StatusOK)

	// Single use.
	expect(t, e.client().do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token, "name": "Again", "password": otherPassword}), http.StatusNotFound)
	if got := decode[map[string][]Invite](t, owner.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/invites", ws), nil)); len(got["invites"]) != 0 {
		t.Fatalf("pending invites after acceptance = %v", got)
	}

	members := decode[map[string][]Member](t, owner.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", ws), nil))["members"]
	if len(members) != 2 || members[0].Name != "New Person" || members[0].Role != RoleMember || members[1].Role != RoleAdmin {
		t.Fatalf("members = %+v", members)
	}
	// Members see the member list too.
	expect(t, newcomer.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", ws), nil), http.StatusOK)

	// Inviting someone who is already a member is refused.
	expect(t, owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", ws), map[string]string{"email": "new@example.com"}), http.StatusConflict)
}

func TestInviteExistingUser(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	member, joined := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")

	// The member runs a second workspace and invites the owner to it.
	ws2 := e.addWorkspace(joined.User.ID, "Side project")
	token := e.invite(member, ws2, "owner@example.com", RoleAdmin)

	// Without logging in, the owner is told to log in rather than creating an account.
	anon := e.client()
	expect(t, anon.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token, "name": "Owner", "password": otherPassword}), http.StatusConflict)
	// Someone else, logged in, can't use it.
	expect(t, member.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token}), http.StatusForbidden)

	rec := owner.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token})
	expect(t, rec, http.StatusOK)
	got := decode[me](t, rec)
	if len(got.Workspaces) != 2 || got.Workspaces[1].WorkspaceID != ws2 || got.Workspaces[1].Role != RoleAdmin {
		t.Fatalf("workspaces after joining = %+v", got.Workspaces)
	}
	if n := e.count(`SELECT count(*) FROM users`); n != 2 {
		t.Fatalf("users = %d, want no new account", n)
	}
	// Switching workspaces is using the other ID.
	expect(t, owner.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", ws2), nil), http.StatusOK)

	// An invite for someone who has since joined another way doesn't add them twice.
	again := e.invite(member, e.addWorkspace(joined.User.ID, "Third"), "owner@example.com", RoleMember)
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) SELECT workspace_id, ?, 'member' FROM invites WHERE token_hash = ?`,
		got.User.ID, hashToken(again)); err != nil {
		t.Fatal(err)
	}
	expect(t, owner.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": again}), http.StatusConflict)
	if n := e.count(`SELECT count(*) FROM invites WHERE token_hash = ? AND accepted_at IS NULL`, hashToken(again)); n != 1 {
		t.Fatal("a refused acceptance used up the invite")
	}
}

func TestInviteExpiresAndCanBeWithdrawn(t *testing.T) {
	e := newEnv(t, Options{InviteTTL: time.Hour})
	owner, ws := e.setup()

	expired := e.invite(owner, ws, "late@example.com", RoleMember)
	e.advance(time.Hour)
	expect(t, e.client().do(http.MethodPost, "/api/invites/accept", map[string]string{"token": expired, "name": "Late", "password": otherPassword}), http.StatusNotFound)

	withdrawn := e.invite(owner, ws, "maybe@example.com", RoleMember)
	list := decode[map[string][]Invite](t, owner.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/invites", ws), nil))["invites"]
	if len(list) != 1 || list[0].Email != "maybe@example.com" {
		t.Fatalf("pending invites = %+v, want only the unexpired one", list)
	}
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/invites/%d", ws, list[0].ID), nil), http.StatusNoContent)
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/invites/%d", ws, list[0].ID), nil), http.StatusNotFound)
	expect(t, e.client().do(http.MethodPost, "/api/invites/accept", map[string]string{"token": withdrawn, "name": "Maybe", "password": otherPassword}), http.StatusNotFound)
}

func TestInviteValidation(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	path := fmt.Sprintf("/api/workspaces/%d/invites", ws)
	expect(t, owner.do(http.MethodPost, path, map[string]string{"email": "nope"}), http.StatusBadRequest)
	expect(t, owner.do(http.MethodPost, path, map[string]string{"email": "a@example.com", "role": "owner"}), http.StatusBadRequest)
	rec := owner.do(http.MethodPost, path, map[string]string{"email": "a@example.com"})
	expect(t, rec, http.StatusCreated)
	if got := decode[Invite](t, rec); got.Role != RoleMember || got.ExpiresAt != "2026-01-09T03:04:05.000Z" {
		t.Fatalf("invite = %+v, want role member, expiring in 7 days", got)
	}
}

func TestMembersAndRoles(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := decode[me](t, owner.do(http.MethodGet, "/api/me", nil)).User.ID
	member, joined := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	memberID := joined.User.ID
	memberPath := func(id int64) string { return fmt.Sprintf("/api/workspaces/%d/members/%d", ws, id) }

	// The only admin can't step down or leave.
	expect(t, owner.do(http.MethodPatch, memberPath(ownerID), map[string]string{"role": "member"}), http.StatusConflict)
	expect(t, owner.do(http.MethodDelete, memberPath(ownerID), nil), http.StatusConflict)

	// Members can't do admin actions.
	expect(t, member.do(http.MethodPatch, memberPath(memberID), map[string]string{"role": "admin"}), http.StatusForbidden)
	expect(t, member.do(http.MethodPatch, memberPath(ownerID), map[string]string{"role": "member"}), http.StatusForbidden)
	expect(t, member.do(http.MethodDelete, memberPath(ownerID), nil), http.StatusForbidden)
	expect(t, member.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", ws), map[string]string{"email": "x@example.com"}), http.StatusForbidden)

	expect(t, owner.do(http.MethodPatch, memberPath(memberID), map[string]string{"role": "boss"}), http.StatusBadRequest)
	expect(t, owner.do(http.MethodPatch, memberPath(memberID), map[string]string{"role": "admin"}), http.StatusNoContent)
	// With two admins, either can step down; then the other is the last.
	expect(t, member.do(http.MethodPatch, memberPath(ownerID), map[string]string{"role": "member"}), http.StatusNoContent)
	expect(t, member.do(http.MethodPatch, memberPath(memberID), map[string]string{"role": "member"}), http.StatusConflict)
	expect(t, owner.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/invites", ws), nil), http.StatusForbidden)

	// Removing a member takes their access away at once.
	expect(t, member.do(http.MethodDelete, memberPath(ownerID), nil), http.StatusNoContent)
	expect(t, owner.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", ws), nil), http.StatusNotFound)
	if got := decode[me](t, owner.do(http.MethodGet, "/api/me", nil)); len(got.Workspaces) != 0 {
		t.Fatalf("removed user's workspaces = %+v", got.Workspaces)
	}
	expect(t, member.do(http.MethodDelete, memberPath(ownerID), nil), http.StatusNotFound)
}

// TestRouteAccess calls every route that needs a login without one, as a user of another
// workspace, and as a plain member where admins only are allowed.
func TestRouteAccess(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	ownerID := decode[me](t, owner.do(http.MethodGet, "/api/me", nil)).User.ID

	// An outsider: admin of their own workspace, not a member of ws.
	outsider, outsiderMe := e.join(e.invite(owner, ws, "outsider@example.com", RoleMember), "Outsider")
	other := e.addWorkspace(outsiderMe.User.ID, "Elsewhere")
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, outsiderMe.User.ID), nil), http.StatusNoContent)
	inviteID := decode[Invite](t, owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", ws), map[string]string{"email": "p@example.com"})).ID

	type route struct {
		method, path string
		body         any
		adminOnly    bool
	}
	w := fmt.Sprintf("/api/workspaces/%d", ws)
	routes := []route{
		{http.MethodGet, w + "/members", nil, false},
		{http.MethodPatch, fmt.Sprintf("%s/members/%d", w, ownerID), map[string]string{"role": "member"}, true},
		{http.MethodDelete, fmt.Sprintf("%s/members/%d", w, ownerID), nil, true},
		{http.MethodGet, w + "/invites", nil, true},
		{http.MethodPost, w + "/invites", map[string]string{"email": "q@example.com"}, true},
		{http.MethodDelete, fmt.Sprintf("%s/invites/%d", w, inviteID), nil, true},
		{http.MethodPatch, w, map[string]string{"locale": "pt-BR"}, true},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			expect(t, e.client().do(rt.method, rt.path, rt.body), http.StatusUnauthorized)
			expect(t, outsider.do(rt.method, rt.path, rt.body), http.StatusNotFound)
			if rt.adminOnly {
				expect(t, member.do(rt.method, rt.path, rt.body), http.StatusForbidden)
			}
		})
	}
	expect(t, e.client().do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	expect(t, e.client().do(http.MethodPatch, "/api/me", map[string]string{"locale": "pt-BR"}), http.StatusUnauthorized)

	// A nonexistent or malformed workspace looks the same as someone else's.
	for _, p := range []string{"/api/workspaces/999/members", "/api/workspaces/abc/members"} {
		expect(t, owner.do(http.MethodGet, p, nil), http.StatusNotFound)
	}
	// An admin of ws can't reach into another workspace through a member or invite ID.
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", other, outsiderMe.User.ID), nil), http.StatusNotFound)
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("%s/members/%d", w, outsiderMe.User.ID), nil), http.StatusNotFound)
	otherInvite := decode[Invite](t, outsider.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", other), map[string]string{"email": "r@example.com"})).ID
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("%s/invites/%d", w, otherInvite), nil), http.StatusNotFound)
	if n := e.count(`SELECT count(*) FROM invites WHERE id = ?`, otherInvite); n != 1 {
		t.Fatal("an admin of one workspace deleted another workspace's invite")
	}
	// Nothing the refused calls asked for happened.
	if n := e.count(`SELECT count(*) FROM memberships WHERE workspace_id = ? AND user_id = ? AND role = 'admin'`, ws, ownerID); n != 1 {
		t.Fatal("a refused call changed the owner's membership")
	}
	if n := e.count(`SELECT count(*) FROM workspaces WHERE locale != ''`); n != 0 {
		t.Fatal("a refused call changed a workspace's language")
	}
}

func TestMiddlewarePutsTheCallerOnTheContext(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	var gotUser User
	var gotMembership Membership
	mux := http.NewServeMux()
	mux.Handle("GET /api/workspaces/{workspace}/probe", e.svc.RequireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, _ = UserFrom(r.Context())
		gotMembership, _ = MembershipFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, owner.req(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/probe", ws), nil))
	expect(t, rec, http.StatusNoContent)
	if gotUser.Email != "owner@example.com" || gotMembership != (Membership{WorkspaceID: ws, WorkspaceName: "Home", Role: RoleAdmin}) {
		t.Fatalf("context: user %+v, membership %+v", gotUser, gotMembership)
	}
	if _, ok := UserFrom(context.Background()); ok {
		t.Fatal("UserFrom found a user on an empty context")
	}
}

// A request without a body (logout, DELETE) needs no content type from the same origin.
func TestCSRFAllowsSameOriginBodilessRequests(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	r := owner.req(http.MethodPost, "/api/logout", nil)
	r.Header.Del("Content-Type")
	expect(t, owner.send(r), http.StatusNoContent)
}

// A server-side failure during login isn't a failed attempt, so it never adds up to 429.
func TestLoginServerErrorsDontCountAgainstTheLimit(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	if _, err := e.db.Exec(`UPDATE users SET password_hash = 'not-a-valid-hash' WHERE email = 'owner@example.com'`); err != nil {
		t.Fatal(err)
	}
	c := e.client()
	for range accountAndIPAttempts + 2 {
		expect(t, c.do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": ownerPassword}), http.StatusInternalServerError)
	}
}

// The routes named as taking a raw body accept application/octet-stream, and only they:
// any other route, or another method on theirs, still needs JSON, and the cross-origin
// check applies to them all.
func TestCSRFRawBodyRoutes(t *testing.T) {
	h := CSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
		"PUT /api/files/{path...}")
	tests := []struct {
		method, target, site string
		status               int
	}{
		{http.MethodPut, "/api/files/a/b.mp4", "same-origin", http.StatusNoContent},
		{http.MethodPut, "/api/files/a/b.mp4", "cross-site", http.StatusForbidden},
		{http.MethodPost, "/api/files/a/b.mp4", "same-origin", http.StatusUnsupportedMediaType},
		{http.MethodPut, "/api/other", "same-origin", http.StatusUnsupportedMediaType},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(tt.method, tt.target, strings.NewReader("bytes"))
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Header.Set("Sec-Fetch-Site", tt.site)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != tt.status {
			t.Errorf("%s %s (%s): status %d, want %d", tt.method, tt.target, tt.site, rec.Code, tt.status)
		}
	}
}
