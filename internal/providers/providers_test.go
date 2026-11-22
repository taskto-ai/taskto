package providers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/serverkey"
)

// setupToken is the first-run setup token the tests' server is given.
var setupToken = auth.NewSetupToken()

// Obviously fake keys and passwords.
const (
	testKey   = "fake-provider-key-for-tests-not-real"
	otherKey  = "fake-rotated-key-for-tests-not-real"
	password  = "fake-password-for-tests"
	remoteURL = "https://llm.example.com/v1"
	localURL  = "http://127.0.0.1:11434/v1"
)

// logs captures everything the package (and the auth middleware) logs.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type env struct {
	t     *testing.T
	db    *sql.DB
	store *Store
	feed  *Feed
	api   *Service
	h     http.Handler
	logs  *logs

	mu      sync.Mutex
	reloads int
	reload  error // what the fake agent runtime answers to a reload

	bodies bytes.Buffer // every response body
}

func newEnv(t *testing.T) *env {
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
	key, err := serverkey.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: sqlDB, store: NewStore(sqlDB, key), logs: &logs{}}
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prev := slog.Default()
	slog.SetDefault(log)
	t.Cleanup(func() { slog.SetDefault(prev) })

	e.feed, err = NewFeed(e.store, FeedConfig{Dir: filepath.Join(dir, "agent"), Reload: e.fakeReload, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a := auth.New(sqlDB, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
	a.Routes(mux)
	e.api = New(e.store, e.feed)
	e.api.Routes(mux, a)
	e.h = auth.CSRF(mux)
	return e
}

func (e *env) fakeReload(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reloads++
	return e.reload
}

func (e *env) reloadCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reloads
}

// client is a browser with a session.
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
		raw, err := json.Marshal(body)
		if err != nil {
			c.e.t.Fatal(err)
		}
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
	c.e.mu.Lock()
	c.e.bodies.Write(rec.Body.Bytes())
	c.e.mu.Unlock()
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

// setup creates the owner, admin of the first workspace, and returns them and its ID.
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

// invite adds a new person to the workspace with role, and returns them logged in.
func (e *env) invite(admin *client, ws int64, email, role string) *client {
	e.t.Helper()
	rec := admin.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", ws), map[string]string{"email": email, "role": role})
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

func account(name, baseURL string, key *string) map[string]any {
	body := map[string]any{
		"name": name, "base_url": baseURL,
		"models": []map[string]any{{"id": "model-a"}, {"id": "model-b", "name": "Model B", "context_window": 32768, "reasoning": true}},
	}
	if key != nil {
		body["api_key"] = *key
	}
	return body
}

func ptr[T any](v T) *T { return &v }

func wsPath(ws int64, rest string) string { return fmt.Sprintf("/api/workspaces/%d/%s", ws, rest) }

func TestWorkspaceProvidersAdminsWriteMembersRead(t *testing.T) {
	e := newEnv(t)
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com", "member")
	outsiderEnv := e.invite(owner, ws, "temp@example.com", "member")
	expect(t, owner.do(http.MethodDelete, wsPath(ws, fmt.Sprintf("members/%d", outsiderEnv.id)), nil), http.StatusNoContent)
	outsider := outsiderEnv

	rec := owner.do(http.MethodPost, wsPath(ws, "providers"), account("gateway", remoteURL, ptr(testKey)))
	expect(t, rec, http.StatusCreated)
	created := decode[Account](t, rec)
	if created.Provider != fmt.Sprintf("ws%d-gateway", ws) || created.Scope != ScopeWorkspace || created.APIKey != "set" ||
		created.Kind != KindOpenAICompatible || created.DisplayName != "gateway" || created.Models[0].Name != "model-a" {
		t.Fatalf("created = %+v", created)
	}

	// A member reads, but can't create, change or delete.
	rec = member.do(http.MethodGet, wsPath(ws, "providers"), nil)
	expect(t, rec, http.StatusOK)
	if list := decode[struct{ Providers []Account }](t, rec).Providers; len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("member's list = %+v", list)
	}
	one := wsPath(ws, fmt.Sprintf("providers/%d", created.ID))
	expect(t, member.do(http.MethodPost, wsPath(ws, "providers"), account("mine", localURL, nil)), http.StatusForbidden)
	expect(t, member.do(http.MethodPatch, one, map[string]any{"display_name": "Hijacked"}), http.StatusForbidden)
	expect(t, member.do(http.MethodPatch, one, map[string]any{"base_url": "https://attacker.example.com/v1"}), http.StatusForbidden)
	expect(t, member.do(http.MethodDelete, one, nil), http.StatusForbidden)

	// Someone outside the workspace can't even tell it exists.
	for _, call := range []struct{ method, path string }{
		{http.MethodGet, wsPath(ws, "providers")},
		{http.MethodPost, wsPath(ws, "providers")},
		{http.MethodPatch, one},
		{http.MethodDelete, one},
		{http.MethodGet, wsPath(ws, "models")},
	} {
		expect(t, outsider.do(call.method, call.path, account("x", localURL, nil)), http.StatusNotFound)
	}
	expect(t, (&client{e: e}).do(http.MethodGet, wsPath(ws, "providers"), nil), http.StatusUnauthorized)

	// Nothing changed.
	rec = owner.do(http.MethodGet, wsPath(ws, "providers"), nil)
	if list := decode[struct{ Providers []Account }](t, rec).Providers; len(list) != 1 || list[0].DisplayName != "gateway" ||
		list[0].BaseURL != remoteURL {
		t.Fatalf("after the refused writes: %+v", list)
	}

	// The admin changes and deletes.
	rec = owner.do(http.MethodPatch, one, map[string]any{"display_name": "Gateway", "models": []map[string]any{{"id": "only"}}})
	expect(t, rec, http.StatusOK)
	if got := decode[Account](t, rec); got.DisplayName != "Gateway" || len(got.Models) != 1 || got.APIKey != "set" || got.BaseURL != remoteURL {
		t.Fatalf("after the patch: %+v", got)
	}
	expect(t, owner.do(http.MethodDelete, one, nil), http.StatusNoContent)
	expect(t, owner.do(http.MethodDelete, one, nil), http.StatusNotFound)
}

// A workspace's account IDs don't reach another workspace's routes.
func TestAccountsStayInTheirWorkspace(t *testing.T) {
	e := newEnv(t)
	owner, ws := e.setup()
	// A second workspace, owned by the same admin: created directly, as there's no API yet.
	res, err := e.db.Exec(`INSERT INTO workspaces (name) VALUES ('Other')`)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := res.LastInsertId()
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, other, owner.id); err != nil {
		t.Fatal(err)
	}
	rec := owner.do(http.MethodPost, wsPath(ws, "providers"), account("one", localURL, nil))
	expect(t, rec, http.StatusCreated)
	id := decode[Account](t, rec).ID
	expect(t, owner.do(http.MethodPatch, wsPath(other, fmt.Sprintf("providers/%d", id)), map[string]any{"display_name": "x"}), http.StatusNotFound)
	expect(t, owner.do(http.MethodDelete, wsPath(other, fmt.Sprintf("providers/%d", id)), nil), http.StatusNotFound)
	rec = owner.do(http.MethodGet, wsPath(other, "providers"), nil)
	if list := decode[struct{ Providers []Account }](t, rec).Providers; len(list) != 0 {
		t.Fatalf("the other workspace lists %+v", list)
	}
	// The same name is free in another workspace, and taken twice in one.
	expect(t, owner.do(http.MethodPost, wsPath(other, "providers"), account("one", localURL, nil)), http.StatusCreated)
	expect(t, owner.do(http.MethodPost, wsPath(ws, "providers"), account("one", localURL, nil)), http.StatusConflict)
}

func TestPersonalProvidersAreInvisibleToOthers(t *testing.T) {
	e := newEnv(t)
	owner, ws := e.setup()
	alice := e.invite(owner, ws, "alice@example.com", "member")
	bob := e.invite(owner, ws, "bob@example.com", "member")

	expect(t, owner.do(http.MethodPost, wsPath(ws, "providers"), account("shared", localURL, nil)), http.StatusCreated)
	rec := alice.do(http.MethodPost, "/api/me/providers", account("mine", remoteURL, ptr(testKey)))
	expect(t, rec, http.StatusCreated)
	mine := decode[Account](t, rec)
	if mine.Provider != fmt.Sprintf("u%d-mine", alice.id) || mine.Scope != ScopeUser {
		t.Fatalf("alice's account = %+v", mine)
	}
	// Bob may use the same name for his own.
	expect(t, bob.do(http.MethodPost, "/api/me/providers", account("mine", localURL, nil)), http.StatusCreated)

	// Bob (and the admin) see only their own personal accounts, and can't touch Alice's.
	for _, other := range []*client{bob, owner} {
		rec = other.do(http.MethodGet, "/api/me/providers", nil)
		for _, a := range decode[struct{ Providers []Account }](t, rec).Providers {
			if a.ID == mine.ID {
				t.Fatalf("user %d lists alice's account", other.id)
			}
		}
		one := fmt.Sprintf("/api/me/providers/%d", mine.ID)
		expect(t, other.do(http.MethodPatch, one, map[string]any{"base_url": "https://attacker.example.com/v1"}), http.StatusNotFound)
		expect(t, other.do(http.MethodDelete, one, nil), http.StatusNotFound)
		// Nor reach it as a workspace account.
		expect(t, owner.do(http.MethodDelete, wsPath(ws, fmt.Sprintf("providers/%d", mine.ID)), nil), http.StatusNotFound)

		rec = other.do(http.MethodGet, wsPath(ws, "models"), nil)
		expect(t, rec, http.StatusOK)
		for _, c := range decode[struct{ Providers []Choice }](t, rec).Providers {
			if c.Provider == mine.Provider {
				t.Fatalf("user %d's model list has alice's provider", other.id)
			}
		}
	}

	// Alice's list in the workspace: the instance's, the workspace's, then her own.
	rec = alice.do(http.MethodGet, wsPath(ws, "models"), nil)
	expect(t, rec, http.StatusOK)
	var got []string
	for _, c := range decode[struct{ Providers []Choice }](t, rec).Providers {
		got = append(got, string(c.Scope)+":"+c.Provider)
	}
	want := []string{"instance:script", fmt.Sprintf("workspace:ws%d-shared", ws), fmt.Sprintf("user:u%d-mine", alice.id)}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("alice's models = %v, want %v", got, want)
	}
	expect(t, alice.do(http.MethodPatch, fmt.Sprintf("/api/me/providers/%d", mine.ID), map[string]any{"display_name": "Mine"}), http.StatusOK)
	expect(t, alice.do(http.MethodDelete, fmt.Sprintf("/api/me/providers/%d", mine.ID), nil), http.StatusNoContent)
}

func TestValidation(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.setup()
	with := func(change func(map[string]any)) map[string]any {
		body := account("p", localURL, nil)
		change(body)
		return body
	}
	cases := map[string]struct {
		body map[string]any
		want string
	}{
		"bad name":             {with(func(b map[string]any) { b["name"] = "Has Spaces" }), "name must be"},
		"name too long":        {with(func(b map[string]any) { b["name"] = strings.Repeat("a", 41) }), "name must be"},
		"unknown kind":         {with(func(b map[string]any) { b["kind"] = "anthropic" }), "kind must be"},
		"not http":             {with(func(b map[string]any) { b["base_url"] = "ftp://example.com/v1" }), "http or https URL"},
		"no host":              {with(func(b map[string]any) { b["base_url"] = "http:///v1" }), "http or https URL"},
		"a query":              {with(func(b map[string]any) { b["base_url"] = localURL + "?x=1" }), "without a query"},
		"bad port":             {with(func(b map[string]any) { b["base_url"] = "http://127.0.0.1:99999/v1" }), "http or https URL"},
		"bad IPv4":             {with(func(b map[string]any) { b["base_url"] = "http://999.1.1.1/v1" }), "http or https URL"},
		"credentials in URL":   {with(func(b map[string]any) { b["base_url"] = "https://user:" + testKey + "@example.com/v1" }), "must not contain credentials"},
		"key over plain http":  {with(func(b map[string]any) { b["base_url"] = "http://models.example.com/v1"; b["api_key"] = testKey }), "must use https"},
		"short key":            {with(func(b map[string]any) { b["api_key"] = "short" }), "api_key must be"},
		"key with a newline":   {with(func(b map[string]any) { b["api_key"] = "fake-key\nsecond-line" }), "api_key must be"},
		"no models":            {with(func(b map[string]any) { b["models"] = []any{} }), "models must list"},
		"duplicate models":     {with(func(b map[string]any) { b["models"] = []map[string]any{{"id": "a"}, {"id": "a"}} }), "is a duplicate"},
		"negative tokens":      {with(func(b map[string]any) { b["models"] = []map[string]any{{"id": "a", "max_tokens": -1}} }), "max_tokens"},
		"compat not object":    {with(func(b map[string]any) { b["compat"] = []int{1} }), "compat must be an object"},
		"unknown compat":       {with(func(b map[string]any) { b["compat"] = map[string]any{"supportsDeveloperrole": true} }), "unknown or unsupported"},
		"nested compat":        {with(func(b map[string]any) { b["compat"] = map[string]any{"openRouterRouting": map[string]any{}} }), "unknown or unsupported"},
		"compat wrong type":    {with(func(b map[string]any) { b["compat"] = map[string]any{"supportsStore": testKey} }), "compat.supportsStore"},
		"compat bad enum":      {with(func(b map[string]any) { b["compat"] = map[string]any{"maxTokensField": "tokens"} }), "compat.maxTokensField"},
		"key in a wrong field": {with(func(b map[string]any) { b["key"] = testKey }), "invalid JSON body"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := owner.do(http.MethodPost, "/api/me/providers", tc.body)
			expect(t, rec, http.StatusBadRequest)
			body := decode[map[string]any](t, rec)
			if msg, _ := body["error"].(string); !strings.Contains(msg, tc.want) {
				t.Fatalf("error = %q, want it to say %q", msg, tc.want)
			}
			if code, _ := body["code"].(string); code == "" {
				t.Fatalf("no code in %v", body)
			}
		})
	}

	// Accepted: a key over plain http to this machine, every kind of compat setting, a trailing
	// slash (dropped).
	ok := with(func(b map[string]any) {
		b["base_url"] = "http://localhost:8000/v1/"
		b["api_key"] = "  " + testKey + "\n"
		b["compat"] = map[string]any{"supportsStore": false, "maxTokensField": "max_tokens", "vllmPriority": 2}
	})
	rec := owner.do(http.MethodPost, "/api/me/providers", ok)
	expect(t, rec, http.StatusCreated)
	got := decode[Account](t, rec)
	if got.BaseURL != "http://localhost:8000/v1" || string(got.Compat) != `{"maxTokensField":"max_tokens","supportsStore":false,"vllmPriority":2}` {
		t.Fatalf("accepted account = %+v (compat %s)", got, got.Compat)
	}
	one := fmt.Sprintf("/api/me/providers/%d", got.ID)
	// The name and kind can't change, and an update is validated whole: no key over remote http.
	expect(t, owner.do(http.MethodPatch, one, map[string]any{"name": "renamed"}), http.StatusBadRequest)
	expect(t, owner.do(http.MethodPatch, one, map[string]any{"base_url": "http://models.example.com/v1"}), http.StatusBadRequest)
	// Without the key, the remote http endpoint is fine.
	rec = owner.do(http.MethodPatch, one, map[string]any{"api_key": nil, "base_url": "http://models.example.com/v1", "compat": nil})
	expect(t, rec, http.StatusOK)
	if got := decode[Account](t, rec); got.APIKey != "not set" || got.Compat != nil {
		t.Fatalf("after removing the key and compat: %+v", got)
	}
}

// The key goes in, and never comes out: not in a response, not in a log line, not in the
// providers file. Only its own key file holds it.
func TestKeysNeverLeave(t *testing.T) {
	e := newEnv(t)
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com", "member")

	rec := owner.do(http.MethodPost, wsPath(ws, "providers"), account("gateway", remoteURL, ptr(testKey)))
	expect(t, rec, http.StatusCreated)
	id := decode[Account](t, rec).ID
	one := wsPath(ws, fmt.Sprintf("providers/%d", id))
	owner.do(http.MethodGet, wsPath(ws, "providers"), nil)
	member.do(http.MethodGet, wsPath(ws, "providers"), nil)
	member.do(http.MethodGet, wsPath(ws, "models"), nil)
	// Refused writes that carry a key, by a member and as invalid input.
	member.do(http.MethodPatch, one, map[string]any{"api_key": otherKey})
	owner.do(http.MethodPost, wsPath(ws, "providers"), account("bad", "http://models.example.com/v1", ptr(otherKey)))
	owner.do(http.MethodPost, "/api/me/providers", map[string]any{"name": "x", "apiKey": otherKey})
	// Rotation.
	expect(t, owner.do(http.MethodPatch, one, map[string]any{"api_key": otherKey}), http.StatusOK)
	// The agent runtime refuses the file, then doesn't answer.
	e.mu.Lock()
	e.reload = fmt.Errorf("%w: invalid providers config", ErrRefused)
	e.mu.Unlock()
	owner.do(http.MethodPatch, one, map[string]any{"display_name": "Refused"})
	e.mu.Lock()
	e.reload = errors.New("connection refused")
	e.mu.Unlock()
	owner.do(http.MethodPatch, one, map[string]any{"display_name": "Unanswered"})

	providersFile, err := os.ReadFile(e.feed.Path())
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := e.db.QueryRow(`SELECT api_key FROM provider_accounts WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	bodies := e.bodies.String()
	e.mu.Unlock()
	for name, text := range map[string]string{
		"a response": bodies, "the log": e.logs.String(), "the providers file": string(providersFile), "the database": string(stored),
	} {
		for _, key := range []string{testKey, otherKey} {
			if strings.Contains(text, key) {
				t.Errorf("%s contains the key %q", name, key)
			}
		}
	}
	if !strings.Contains(e.logs.String(), "refused the providers file") {
		t.Error("the refused reload wasn't logged; that path went untested")
	}
	keyFile, err := os.ReadFile(filepath.Join(e.feed.keyDir(), fmt.Sprintf("ws%d-gateway.key", ws)))
	if err != nil || string(keyFile) != otherKey {
		t.Fatalf("key file = %q, %v; want the rotated key", keyFile, err)
	}
}

func TestFeedFiles(t *testing.T) {
	e := newEnv(t)
	owner, ws := e.setup()
	before := e.reloadCount()

	rec := owner.do(http.MethodPost, wsPath(ws, "providers"), map[string]any{
		"name": "gateway", "display_name": "Team gateway", "base_url": remoteURL, "api_key": testKey,
		"compat": map[string]any{"supportsStore": false},
		"models": []map[string]any{{"id": "big", "name": "Big", "context_window": 200000, "max_tokens": 4096, "reasoning": true}},
	})
	expect(t, rec, http.StatusCreated)
	gateway := decode[Account](t, rec)
	rec = owner.do(http.MethodPost, "/api/me/providers", account("local", localURL, nil))
	expect(t, rec, http.StatusCreated)
	if e.reloadCount() != before+2 {
		t.Fatalf("reloads = %d, want one per change", e.reloadCount()-before)
	}

	keyPath := filepath.Join(e.feed.keyDir(), gateway.Provider+".key")
	want := fmt.Sprintf(`{"providers":[
		{"id":"script","kind":"script","name":"Scripted (offline)"},
		{"id":"%s","kind":"openai-compatible","name":"Team gateway","baseUrl":"%s","apiKey":{"file":"%s"},"compat":{"supportsStore":false},
		 "models":[{"id":"big","name":"Big","contextWindow":200000,"maxTokens":4096,"reasoning":true}]},
		{"id":"u%d-local","kind":"openai-compatible","name":"local","baseUrl":"%s",
		 "models":[{"id":"model-a","name":"model-a","reasoning":false},{"id":"model-b","name":"Model B","contextWindow":32768,"reasoning":true}]}
	]}`, gateway.Provider, remoteURL, keyPath, owner.id, localURL)
	assertJSON(t, e.feed.Path(), want)

	for path, mode := range map[string]os.FileMode{
		e.feed.dir: 0o700, e.feed.keyDir(): 0o700, e.feed.Path(): 0o600, keyPath: 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Errorf("%s: %v, %v; want mode %o", path, info, err, mode)
		}
	}
	if key, err := os.ReadFile(keyPath); err != nil || string(key) != testKey {
		t.Fatalf("key file = %q, %v", key, err)
	}
	if entries, _ := os.ReadDir(e.feed.keyDir()); len(entries) != 1 {
		t.Fatalf("key directory has %d files, want 1 (no key, no file)", len(entries))
	}

	// Removing the key removes its file; so does deleting the account.
	one := wsPath(ws, fmt.Sprintf("providers/%d", gateway.ID))
	expect(t, owner.do(http.MethodPatch, one, map[string]any{"api_key": nil}), http.StatusOK)
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key file after removing the key: %v", err)
	}
	expect(t, owner.do(http.MethodPatch, one, map[string]any{"api_key": testKey}), http.StatusOK)
	expect(t, owner.do(http.MethodDelete, one, nil), http.StatusNoContent)
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key file after deleting the account: %v", err)
	}
	assertJSON(t, e.feed.Path(), fmt.Sprintf(`{"providers":[
		{"id":"script","kind":"script","name":"Scripted (offline)"},
		{"id":"u%d-local","kind":"openai-compatible","name":"local","baseUrl":"%s",
		 "models":[{"id":"model-a","name":"model-a","reasoning":false},{"id":"model-b","name":"Model B","contextWindow":32768,"reasoning":true}]}
	]}`, owner.id, localURL))
}

// A sealed key copied to another account doesn't open there: that account is left out of the
// agent runtime, with a log line that doesn't quote anything.
func TestMovedKeyDoesNotOpen(t *testing.T) {
	e := newEnv(t)
	owner, ws := e.setup()
	expect(t, owner.do(http.MethodPost, wsPath(ws, "providers"), account("a", remoteURL, ptr(testKey))), http.StatusCreated)
	expect(t, owner.do(http.MethodPost, wsPath(ws, "providers"), account("b", remoteURL, ptr(otherKey))), http.StatusCreated)
	if _, err := e.db.Exec(`UPDATE provider_accounts SET api_key = (SELECT api_key FROM provider_accounts WHERE name = 'a') WHERE name = 'b'`); err != nil {
		t.Fatal(err)
	}
	if err := e.feed.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(e.feed.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), fmt.Sprintf("ws%d-a", ws)) || strings.Contains(string(raw), fmt.Sprintf("ws%d-b", ws)) {
		t.Fatalf("providers file = %s; want a in it and b left out", raw)
	}
	if _, err := os.Stat(filepath.Join(e.feed.keyDir(), fmt.Sprintf("ws%d-b.key", ws))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("b's key file: %v; want it gone", err)
	}
	if !strings.Contains(e.logs.String(), "can't be decrypted") || strings.Contains(e.logs.String(), testKey) {
		t.Fatalf("log = %s", e.logs.String())
	}
}

// The operator's providers come first, as written; account IDs are theirs alone.
func TestOperatorProviders(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	write := func(content string) string {
		path := filepath.Join(dir, "operator.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	operator := `{"id":"gateway","kind":"openai-compatible","name":"Gateway","baseUrl":"https://gw.example.com/v1",` +
		`"apiKey":{"env":"GATEWAY_KEY"},"models":[{"id":"m1"},{"id":"m2","name":"Second"}]}`
	feed, err := NewFeed(e.store, FeedConfig{Dir: filepath.Join(dir, "agent"), OperatorFile: write(`{"providers":[` + operator + `]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := feed.Write(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, feed.Path(), `{"providers":[`+operator+`]}`)
	got := feed.Instance()
	if len(got) != 1 || got[0].Provider != "gateway" || got[0].Name != "Gateway" || got[0].Scope != ScopeInstance ||
		len(got[0].Models) != 2 || got[0].Models[0].Name != "m1" || got[0].Models[1].Name != "Second" {
		t.Fatalf("instance providers = %+v", got)
	}

	for content, want := range map[string]string{
		`{"providers":[{"id":"u3-mine","kind":"script"}]}`:    "looks like a provider account's",
		`{"providers":[{"id":"cli-claude","kind":"script"}]}`: "or a CLI worker's",
		`{"providers":[]}`:                  "non-empty",
		`{"providers":`:                     "not valid JSON",
		`{"providers":["` + testKey + `"]}`: "is not an object",
	} {
		_, err := NewFeed(e.store, FeedConfig{Dir: dir, OperatorFile: write(content)})
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), testKey) {
			t.Errorf("operator file %s: err = %v, want one saying %q without quoting the file", content, err, want)
		}
	}
}

func TestUsable(t *testing.T) {
	const user, workspace = 7, 3
	cases := map[string]bool{
		"ws3-gateway": true,
		"ws4-gateway": false,
		"u7-mine":     true,
		"u8-mine":     false,
		"u77-mine":    false,
		"ws33-x":      false,
		"script":      true,
		"local":       true,
		"wsx-1":       true, // not an account's ID: an operator's
	}
	for provider, want := range cases {
		if got := Usable(provider, user, workspace); got != want {
			t.Errorf("Usable(%q) = %v, want %v", provider, got, want)
		}
	}
}

func assertJSON(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got, exp any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if err := json.Unmarshal([]byte(want), &exp); err != nil {
		t.Fatalf("want: %v", err)
	}
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(exp)
	if !bytes.Equal(g, w) {
		t.Fatalf("%s =\n%s\nwant\n%s", path, g, w)
	}
}

// CLI workers come after the operator's providers, in the file and in the model list, so the
// default model stays the operator's.
func TestCLIProviders(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	feed, err := NewFeed(e.store, FeedConfig{Dir: filepath.Join(dir, "agent"), CLI: []CLIProvider{
		{CLI: "claude", Name: "Claude Code", Models: []ModelChoice{{ID: "default", Name: "Claude Code (its default model)"}, {ID: "opus", Name: "opus"}}},
		{CLI: "codex", Name: "Codex", Models: []ModelChoice{{ID: "default", Name: "Codex (its default model)"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := feed.Write(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, feed.Path(), `{"providers":[`+string(scriptProvider)+
		`,{"id":"cli-claude","kind":"cli","name":"Claude Code","cli":"claude","models":[{"id":"default","name":"Claude Code (its default model)"},{"id":"opus","name":"opus"}]}`+
		`,{"id":"cli-codex","kind":"cli","name":"Codex","cli":"codex","models":[{"id":"default","name":"Codex (its default model)"}]}]}`)
	got := feed.Instance()
	if len(got) != 3 || got[0].Scope != ScopeInstance || got[1].Provider != "cli-claude" || got[1].Scope != ScopeCLI || len(got[1].Models) != 2 || got[2].Provider != "cli-codex" {
		t.Fatalf("instance providers = %+v", got)
	}
	if !Usable("cli-claude", 7, 3) {
		t.Error("a CLI worker isn't usable by everyone: the turn checks the owner's own login")
	}
}
