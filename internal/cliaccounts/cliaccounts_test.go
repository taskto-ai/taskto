//go:build unix

package cliaccounts

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/providers"
	"taskto.ai/taskto/internal/serverkey"
)

func TestMain(m *testing.M) {
	clitest.Main()
	os.Exit(m.Run())
}

var setupToken = auth.NewSetupToken()

const password = "fake-password-for-tests"

type env struct {
	t      *testing.T
	db     *sql.DB
	svc    *Service
	worker *cliworker.Worker
	layout cliworker.Layout
	h      http.Handler
	proxy  *httptest.Server
}

func newEnv(t *testing.T, mode Mode, tune func(*cliworker.Config)) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	sqlDB, err := db.Open(ctx, filepath.Join(dir, db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: sqlDB, layout: cliworker.Layout{Root: filepath.Join(dir, "cli")}}
	var worker Worker
	if mode != Off {
		cfg := cliworker.Config{Layout: e.layout, Bins: map[cliworker.CLI]string{}, Path: "/usr/bin:/bin", StopGrace: time.Second, UnsafePrivileged: os.Geteuid() == 0}
		for name, path := range clitest.Install(t, t.TempDir()) {
			cfg.Bins[cliworker.CLI(name)] = path
		}
		if tune != nil {
			tune(&cfg)
		}
		e.worker, err = cliworker.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(e.worker.Close)
		worker = LocalWorker(e.worker)
	}
	e.svc = New(sqlDB, mode, worker)
	e.svc.poll = 20 * time.Millisecond
	t.Cleanup(e.svc.Close)
	key, err := serverkey.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.proxy = httptest.NewServer(e.svc.ProxyHandler())
	t.Cleanup(e.proxy.Close)
	e.svc.SetShared(Shared{Key: key, ProxyURL: e.proxy.URL})
	store := providers.NewStore(sqlDB, key)
	feed, err := providers.NewFeed(store, providers.FeedConfig{Dir: filepath.Join(dir, "agent")})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.SetModelCatalog(providers.New(store, feed))
	mux := http.NewServeMux()
	a := auth.New(sqlDB, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
	a.Routes(mux)
	e.svc.Routes(mux, a)
	e.svc.SharedRoutes(mux, a)
	e.svc.SwitchingRoutes(mux, a)
	e.svc.OperatorRoutes(mux, a)
	e.h = auth.CSRF(mux)
	return e
}

func (e *env) acct(user int64) cliowner.Account {
	e.t.Helper()
	a, err := e.svc.Account(context.Background(), user)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) dirs(user int64) cliworker.Dirs { return e.dirsSlot(user, "", "") }

func (e *env) dirsSlot(user int64, cli cliworker.CLI, slot cliworker.Slot) cliworker.Dirs {
	e.t.Helper()
	d, err := e.worker.AccountDirs(e.acct(user), cli, slot)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
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

func (e *env) setup() (*client, int64) {
	e.t.Helper()
	c := &client{e: e}
	rec := c.do(http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": password, "workspace_name": "Home", "setup_token": setupToken,
	})
	expect(e.t, rec, http.StatusCreated)
	m := decode[me](e.t, rec)
	c.id = m.User.ID
	return c, m.Workspaces[0].ID
}

func (e *env) invite(admin *client, ws int64, email string) *client {
	e.t.Helper()
	rec := admin.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", ws), map[string]string{"email": email, "role": "member"})
	expect(e.t, rec, http.StatusCreated)
	token := decode[struct {
		Token string `json:"token"`
	}](e.t, rec).Token
	c := &client{e: e}
	rec = c.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token, "name": email, "password": password})
	expect(e.t, rec, http.StatusCreated)
	c.id = decode[me](e.t, rec).User.ID
	return c
}

const base = "/api/me/cli-accounts"

func (c *client) list() list {
	c.e.t.Helper()
	rec := c.do(http.MethodGet, base, nil)
	expect(c.e.t, rec, http.StatusOK)
	return decode[list](c.e.t, rec)
}

func (c *client) account(cli cliworker.CLI) Account {
	c.e.t.Helper()
	return c.accountByID(string(cli))
}

func (c *client) accountByID(id string) Account {
	c.e.t.Helper()
	for _, a := range c.list().Accounts {
		if a.ID == id {
			return a
		}
	}
	c.e.t.Fatalf("no account %s", id)
	return Account{}
}

func (c *client) startLogin(cli cliworker.CLI) cliworker.LoginInfo {
	c.e.t.Helper()
	return c.startLoginOf(string(cli))
}

func (c *client) startLoginOf(id string) cliworker.LoginInfo {
	c.e.t.Helper()
	rec := c.do(http.MethodPost, base+"/"+id+"/login", map[string]any{})
	expect(c.e.t, rec, http.StatusOK)
	return decode[cliworker.LoginInfo](c.e.t, rec)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *client) connect(cli cliworker.CLI) {
	c.e.t.Helper()
	c.connectAccount(string(cli))
}

func (c *client) connectAccount(id string) {
	c.e.t.Helper()
	a, ok := parseHandle(id)
	if !ok {
		c.e.t.Fatalf("bad handle %s", id)
	}
	info := c.startLoginOf(id)
	if info.State != cliworker.LoginWaiting || info.URL == "" {
		c.e.t.Fatalf("login = %+v", info)
	}
	if a.cli == cliworker.Claude {
		expect(c.e.t, c.do(http.MethodPost, base+"/"+id+"/login/code", map[string]string{"code": clitest.GoodCode}), http.StatusAccepted)
	} else {
		clitest.Approve(c.e.t, c.e.dirs(c.id).ConfigOf(a.cli, a.slot))
	}
	eventually(c.e.t, "the account didn't connect", func() bool { return c.accountByID(id).State == Connected })
}

func TestOff(t *testing.T) {
	e := newEnv(t, Off, nil)
	owner, _ := e.setup()
	l := owner.list()
	if l.Enabled || l.Mode != "off" || len(l.Accounts) != 0 || l.Reason == "" {
		t.Fatalf("list = %+v", l)
	}
	expect(t, owner.do(http.MethodPost, base+"/claude/login", map[string]any{}), http.StatusConflict)
}

func TestSignedOut(t *testing.T) {
	e := newEnv(t, Local, nil)
	anon := &client{e: e}
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, base}, {http.MethodPost, base + "/claude/login"}, {http.MethodGet, base + "/claude/login"},
		{http.MethodPost, base + "/claude/login/code"}, {http.MethodDelete, base + "/claude"},
	} {
		var body any
		if r.method == http.MethodPost {
			body = map[string]any{}
		}
		expect(t, anon.do(r.method, r.path, body), http.StatusUnauthorized)
	}
}

func TestLocalModeOnlyWithOneUser(t *testing.T) {
	e := newEnv(t, Local, nil)
	owner, ws := e.setup()
	if l := owner.list(); !l.Enabled || l.Mode != "local" || len(l.Accounts) != 3 {
		t.Fatalf("list = %+v", l)
	}
	owner.connect(cliworker.Codex)

	member := e.invite(owner, ws, "member@example.com")
	for _, c := range []*client{owner, member} {
		l := c.list()
		if l.Enabled || !strings.Contains(l.Reason, "single user") {
			t.Errorf("list with two users = %+v", l)
		}
		rec := c.do(http.MethodPost, base+"/claude/login", map[string]any{})
		expect(t, rec, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), "OS boundary") {
			t.Errorf("refusal = %s", rec.Body.String())
		}
		expect(t, c.do(http.MethodPost, base+"/codex/check", map[string]any{}), http.StatusConflict)
	}
	if err := e.svc.Usable(context.Background()); !errors.Is(err, ErrNoBoundary) {
		t.Errorf("Usable = %v", err)
	}
}

func TestConnectEachCLI(t *testing.T) {
	e := newEnv(t, Local, nil)
	owner, _ := e.setup()
	for _, cli := range cliworker.CLIs {
		t.Run(string(cli), func(t *testing.T) {
			if a := owner.account(cli); a.State != Disconnected || !a.Available || a.Name != cli.Name() {
				t.Fatalf("before: %+v", a)
			}
			owner.connect(cli)
			if a := owner.account(cli); a.Login == nil || a.Login.State != cliworker.LoginConnected {
				t.Errorf("after: %+v", a)
			}
			eventually(t, "no auth method", func() bool { return owner.account(cli).AuthMethod != "" })
		})
	}
}

func TestDeviceLoginRecordedWithoutPolling(t *testing.T) {
	e := newEnv(t, Local, nil)
	owner, _ := e.setup()
	info := owner.startLogin(cliworker.Grok)
	if info.Code != "GR0K-FAKE" || !info.URLTrusted {
		t.Fatalf("login = %+v", info)
	}
	clitest.Approve(t, e.dirs(owner.id).Config(cliworker.Grok))
	eventually(t, "the login wasn't recorded", func() bool {
		var state string
		err := e.db.QueryRow(`SELECT state FROM cli_accounts WHERE user_id = ? AND cli = 'grok'`, owner.id).Scan(&state)
		return err == nil && state == "connected"
	})
}

func TestLoginErrors(t *testing.T) {
	e := newEnv(t, Local, func(c *cliworker.Config) { delete(c.Bins, cliworker.Grok) })
	owner, _ := e.setup()
	expect(t, owner.do(http.MethodPost, base+"/bash/login", map[string]any{}), http.StatusNotFound)
	expect(t, owner.do(http.MethodPost, base+"/grok/login", map[string]any{}), http.StatusServiceUnavailable)
	if a := owner.account(cliworker.Grok); a.Available {
		t.Errorf("grok = %+v", a)
	}
	expect(t, owner.do(http.MethodGet, base+"/claude/login", nil), http.StatusNotFound)
	expect(t, owner.do(http.MethodPost, base+"/claude/login/code", map[string]string{"code": "x"}), http.StatusNotFound)
	owner.startLogin(cliworker.Claude)
	expect(t, owner.do(http.MethodPost, base+"/claude/login/code", map[string]string{"code": "has space"}), http.StatusBadRequest)
	expect(t, owner.do(http.MethodPost, base+"/claude/login/code", map[string]string{"code": "x", "extra": "y"}), http.StatusBadRequest)
	owner.startLogin(cliworker.Codex)
	expect(t, owner.do(http.MethodPost, base+"/codex/login/code", map[string]string{"code": "x"}), http.StatusConflict)
	expect(t, owner.do(http.MethodDelete, base+"/codex/login", nil), http.StatusNoContent)
	eventually(t, "the login wasn't cancelled", func() bool {
		rec := owner.do(http.MethodGet, base+"/codex/login", nil)
		return rec.Code == http.StatusOK && decode[cliworker.LoginInfo](t, rec).State == cliworker.LoginCancelled
	})
	expect(t, owner.do(http.MethodPost, base+"/claude/login/code", map[string]string{"code": "wrong"}), http.StatusAccepted)
	eventually(t, "the login didn't fail", func() bool {
		rec := owner.do(http.MethodGet, base+"/claude/login", nil)
		return rec.Code == http.StatusOK && decode[cliworker.LoginInfo](t, rec).State == cliworker.LoginFailed
	})
	if a := owner.account(cliworker.Claude); a.State != Disconnected {
		t.Errorf("account = %+v", a)
	}
}

func TestAnotherUsersAccounts(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	owner, ws := e.setup()
	other := e.invite(owner, ws, "other@example.com")

	info := owner.startLogin(cliworker.Claude)
	if info.State != cliworker.LoginWaiting {
		t.Fatalf("login = %+v", info)
	}
	expect(t, other.do(http.MethodGet, base+"/claude/login", nil), http.StatusNotFound)
	expect(t, other.do(http.MethodPost, base+"/claude/login/code", map[string]string{"code": clitest.GoodCode}), http.StatusNotFound)
	expect(t, other.do(http.MethodDelete, base+"/claude/login", nil), http.StatusNoContent)
	if a := other.account(cliworker.Claude); a.Login != nil || a.State != Disconnected {
		t.Errorf("the other user's account shows %+v", a)
	}
	rec := owner.do(http.MethodGet, base+"/claude/login", nil)
	expect(t, rec, http.StatusOK)
	if got := decode[cliworker.LoginInfo](t, rec); got.State != cliworker.LoginWaiting {
		t.Fatalf("the owner's login after the other user's attempts: %+v", got)
	}

	expect(t, owner.do(http.MethodPost, base+"/claude/login/code", map[string]string{"code": clitest.GoodCode}), http.StatusAccepted)
	eventually(t, "the owner's account didn't connect", func() bool { return owner.account(cliworker.Claude).State == Connected })

	if a := other.account(cliworker.Claude); a.State != Disconnected {
		t.Errorf("the other user's account = %+v", a)
	}
	rec = other.do(http.MethodPost, base+"/claude/check", map[string]any{})
	expect(t, rec, http.StatusOK)
	if a := decode[Account](t, rec); a.State != Disconnected {
		t.Errorf("the other user's check = %+v", a)
	}
	expect(t, other.do(http.MethodDelete, base+"/claude", nil), http.StatusNoContent)
	if a := owner.account(cliworker.Claude); a.State != Connected {
		t.Errorf("the owner's account after the other user's disconnect: %+v", a)
	}
	if _, err := os.Stat(filepath.Join(e.dirs(owner.id).Config(cliworker.Claude), ".credentials.json")); err != nil {
		t.Errorf("the owner's login is gone: %v", err)
	}
	if st, _ := e.worker.Status(context.Background(), e.acct(other.id), cliworker.Claude, ""); st.LoggedIn {
		t.Error("the other user is signed in with the owner's login")
	}
}

func TestCheckFindsAnExpiredLogin(t *testing.T) {
	e := newEnv(t, Local, nil)
	owner, _ := e.setup()
	owner.connect(cliworker.Claude)
	if err := os.Remove(filepath.Join(e.dirs(owner.id).Config(cliworker.Claude), ".credentials.json")); err != nil {
		t.Fatal(err)
	}
	rec := owner.do(http.MethodPost, base+"/claude/check", map[string]any{})
	expect(t, rec, http.StatusOK)
	if a := decode[Account](t, rec); a.State != Reconnect {
		t.Fatalf("check = %+v", a)
	}
	if a := owner.account(cliworker.Claude); a.State != Reconnect {
		t.Fatalf("account = %+v", a)
	}
	owner.connect(cliworker.Claude)
}

func TestSetReconnectSticks(t *testing.T) {
	e := newEnv(t, Local, nil)
	owner, _ := e.setup()
	owner.connect(cliworker.Codex)
	if err := e.svc.setReconnect(context.Background(), e.accountID(owner.id, cliworker.Codex)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if a := owner.account(cliworker.Codex); a.State != Reconnect {
			t.Fatalf("account = %+v", a)
		}
	}
}

func TestDisconnect(t *testing.T) {
	e := newEnv(t, Local, nil)
	owner, _ := e.setup()
	owner.connect(cliworker.Codex)
	expect(t, owner.do(http.MethodDelete, base+"/codex", nil), http.StatusNoContent)
	if a := owner.account(cliworker.Codex); a.State != Disconnected {
		t.Errorf("account = %+v", a)
	}
	if st, _ := e.worker.Status(context.Background(), e.acct(owner.id), cliworker.Codex, ""); st.LoggedIn {
		t.Error("still signed in")
	}
}

func TestRecordLimitsMergesByWindow(t *testing.T) {
	e := newEnv(t, Local, nil)
	owner, _ := e.setup()
	ctx := context.Background()
	pct := func(v float64) *float64 { return &v }
	if err := e.svc.recordLimits(ctx, e.accountID(owner.id, cliworker.Claude), []cliworker.Limit{{Window: "five_hour", UsedPercent: pct(10)}}); err != nil {
		t.Fatal(err)
	}
	if a := owner.account(cliworker.Claude); len(a.Limits) != 0 {
		t.Fatalf("limits on a disconnected account: %+v", a)
	}
	owner.connect(cliworker.Claude)
	if err := e.svc.recordLimits(ctx, e.accountID(owner.id, cliworker.Claude), []cliworker.Limit{{Window: "five_hour", UsedPercent: pct(10)}, {Window: "seven_day", UsedPercent: pct(3)}}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.recordLimits(ctx, e.accountID(owner.id, cliworker.Claude), []cliworker.Limit{{Window: "five_hour", UsedPercent: pct(55), Status: "allowed_warning"}}); err != nil {
		t.Fatal(err)
	}
	a := owner.account(cliworker.Claude)
	if len(a.Limits) != 2 || a.LimitsAt == nil {
		t.Fatalf("limits = %+v", a)
	}
	for _, l := range a.Limits {
		if (l.Window == "five_hour" && (*l.UsedPercent != 55 || l.Status != "allowed_warning")) || (l.Window == "seven_day" && *l.UsedPercent != 3) {
			t.Errorf("limit = %+v", l)
		}
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": Off, "off": Off, "local": Local, "local-unsafe": LocalUnsafe} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	if got, err := ParseMode("container"); err != nil || got != Container {
		t.Errorf("container: %q %v", got, err)
	}
	if _, err := ParseMode("docker"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

func TestDisconnectIsNotUndoneByTheLogin(t *testing.T) {
	e := newEnv(t, Local, nil)
	owner, _ := e.setup()
	owner.connect(cliworker.Claude)
	expect(t, owner.do(http.MethodDelete, base+"/claude", nil), http.StatusNoContent)
	for range 3 {
		if a := owner.account(cliworker.Claude); a.State != Disconnected {
			t.Fatalf("account = %+v", a)
		}
	}
	if code := owner.do(http.MethodGet, base+"/claude/login", nil).Code; code != http.StatusOK && code != http.StatusNotFound {
		t.Fatalf("login = %d", code)
	}
	if a := owner.account(cliworker.Claude); a.State != Disconnected {
		t.Fatalf("account after reading the login = %+v", a)
	}
}

func TestDisconnectStopsALoginInProgress(t *testing.T) {
	e := newEnv(t, Local, nil)
	owner, _ := e.setup()
	owner.startLogin(cliworker.Codex)
	expect(t, owner.do(http.MethodDelete, base+"/codex", nil), http.StatusNoContent)
	clitest.Approve(t, e.dirs(owner.id).Config(cliworker.Codex))
	time.Sleep(200 * time.Millisecond)
	if st, _ := e.worker.Status(context.Background(), e.acct(owner.id), cliworker.Codex, ""); st.LoggedIn {
		t.Fatal("the cancelled login signed in")
	}
	if a := owner.account(cliworker.Codex); a.State != Disconnected {
		t.Fatalf("account = %+v", a)
	}
}

func (e *env) accountID(user int64, cli cliworker.CLI) int64 {
	e.t.Helper()
	var id int64
	err := e.db.QueryRow(`SELECT id FROM cli_accounts WHERE user_id = ? AND cli = ? AND slot = 1`, user, string(cli)).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) provide(provider string, models ...string) {
	e.t.Helper()
	var name string
	var owner int64
	insert := `INSERT INTO provider_accounts (user_id, name, display_name, kind, base_url, models) VALUES (?, ?, ?, 'openai-compatible', 'https://models.example.com/v1', ?)`
	if _, err := fmt.Sscanf(provider, "ws%d-%s", &owner, &name); err == nil {
		insert = `INSERT INTO provider_accounts (workspace_id, name, display_name, kind, base_url, models) VALUES (?, ?, ?, 'openai-compatible', 'https://models.example.com/v1', ?)`
	} else if _, err := fmt.Sscanf(provider, "u%d-%s", &owner, &name); err != nil {
		e.t.Fatalf("not a provider account: %s", provider)
	}
	list := []map[string]any{}
	for _, m := range models {
		list = append(list, map[string]any{"id": m, "name": m, "reasoning": false})
	}
	raw, _ := json.Marshal(list)
	if _, err := e.db.Exec(insert, owner, name, name, string(raw)); err != nil {
		e.t.Fatal(err)
	}
}
