package dictation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/serverkey"
)

var ctx = context.Background()

// Test passwords and keys: obviously fake.
const (
	fakePassword = "fake-test-password"
	fakeKey      = "fake-speech-api-key"
)

var setupToken = auth.NewSetupToken()

// fakeLocal is a local engine that answers what it is told, and records what it was asked.
type fakeLocal struct {
	mu    sync.Mutex
	state LocalState
	text  string
	err   error
	calls []Options
	warms int
}

func (f *fakeLocal) Model() string { return "fake-model" }
func (f *fakeLocal) State() LocalState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}
func (f *fakeLocal) Warm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.warms++
}
func (f *fakeLocal) Credits() []Credit {
	return []Credit{{Name: "Fake model", By: "Tests", Licence: "CC-BY-4.0"}}
}
func (f *fakeLocal) Transcribe(_ context.Context, a Audio, o Options) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, o)
	if f.state.State != LocalReady {
		return "", localReason(f.state)
	}
	return f.text, f.err
}

// fakeAPI is an OpenAI-compatible transcription endpoint.
type fakeAPI struct {
	*httptest.Server
	mu   sync.Mutex
	last *http.Request
	form map[string]string
	wav  []byte
	text string
	code int
	// redirect, if set, answers 307 to it.
	redirect string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{text: "hello from the api", code: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.last, f.form = r, map[string]string{}
		if f.redirect != "" {
			http.Redirect(w, r, f.redirect, http.StatusTemporaryRedirect)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil { //nolint:gosec // a test server, of the test's own requests
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for k, v := range r.MultipartForm.Value {
			f.form[k] = v[0]
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.wav, _ = io.ReadAll(file)
		if f.code != http.StatusOK {
			http.Error(w, `{"error":"echo `+r.Header.Get("Authorization")+`"}`, f.code)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"text": f.text})
	}))
	t.Cleanup(f.Close)
	return f
}

type env struct {
	t     *testing.T
	h     http.Handler
	db    *sql.DB
	svc   *Service
	local *fakeLocal
	api   *fakeAPI
}

// newEnv serves the account, space, knowledge-base and dictation routes, as the server does.
// local is the local engine, or nil for none.
func newEnv(t *testing.T, local *fakeLocal, policy egress.Policy, more ...func(*Config)) *env {
	t.Helper()
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
	accounts := auth.New(sqlDB, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
	spaces := access.New(sqlDB, accounts)
	kb := knowledge.New(dir, knowledge.Options{})
	cfg := Config{DB: sqlDB, Key: key, Docs: kb, Egress: policy}
	if local != nil {
		cfg.Local = local
	}
	for _, f := range more {
		f(&cfg)
	}
	svc := New(cfg)
	// Tests' names resolve to loopback, which the "public" policy refuses.
	svc.resolve = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	mux := http.NewServeMux()
	accounts.Routes(mux)
	spaces.Routes(mux)
	kb.Routes(mux, spaces)
	svc.Routes(mux, accounts, spaces)
	return &env{t: t, h: auth.CSRF(mux), db: sqlDB, svc: svc, local: local, api: newFakeAPI(t)}
}

type client struct {
	e      *env
	id     int64
	cookie *http.Cookie
}

func (c *client) do(method, target string, body any) *httptest.ResponseRecorder {
	c.e.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, target, nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			c.e.t.Fatal(err)
		}
		r = httptest.NewRequest(method, target, bytes.NewReader(raw))
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

type meReply struct {
	User       auth.User
	Workspaces []auth.Membership
}

// setup makes the owner (an admin) and their workspace.
func (e *env) setup() (*client, int64) {
	owner := &client{e: e}
	rec := owner.do(http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": fakePassword, "workspace_name": "Home", "setup_token": setupToken,
	})
	expect(e.t, rec, http.StatusCreated)
	me := decode[meReply](e.t, rec)
	owner.id = me.User.ID
	return owner, me.Workspaces[0].WorkspaceID
}

// join invites name to ws as a member.
func (e *env) join(owner *client, ws int64, name string) *client {
	rec := owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", ws),
		map[string]string{"email": name + "@example.com", "role": "member"})
	expect(e.t, rec, http.StatusCreated)
	token := decode[struct{ Token string }](e.t, rec).Token
	c := &client{e: e}
	rec = c.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token, "name": name, "password": fakePassword})
	expect(e.t, rec, http.StatusCreated)
	c.id = decode[meReply](e.t, rec).User.ID
	return c
}

// otherWorkspace makes a second workspace with its own admin, created directly, as there's
// no API for it.
func (e *env) otherWorkspace(owner *client, ws int64) (*client, int64) {
	stranger := e.join(owner, ws, "stranger")
	res, err := e.db.Exec(`INSERT INTO workspaces (name) VALUES ('Other')`)
	if err != nil {
		e.t.Fatal(err)
	}
	other, _ := res.LastInsertId()
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, other, stranger.id); err != nil {
		e.t.Fatal(err)
	}
	expect(e.t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, stranger.id), nil), http.StatusNoContent)
	return stranger, other
}

func (c *client) privateSpace(ws int64) access.Space {
	c.e.t.Helper()
	rec := c.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces", ws), nil)
	expect(c.e.t, rec, http.StatusOK)
	for _, s := range decode[struct{ Spaces []access.Space }](c.e.t, rec).Spaces {
		if s.Kind == access.Private {
			return s
		}
	}
	c.e.t.Fatal("no private space")
	return access.Space{}
}

func (c *client) putDoc(sp access.Space, name, content string) {
	c.e.t.Helper()
	rec := c.do(http.MethodPut, fmt.Sprintf("/api/workspaces/%d/spaces/%d/docs/%s", sp.WorkspaceID, sp.ID, name),
		map[string]string{"content": content})
	expect(c.e.t, rec, http.StatusCreated)
}

func route(ws int64, rest string) string {
	return strings.TrimSuffix(fmt.Sprintf("/api/workspaces/%d/dictation/%s", ws, rest), "/")
}

// speech is a second of WAV, base64-encoded, as the web app sends it.
func speech() map[string]any {
	return map[string]any{"audio_base64": base64.StdEncoding.EncodeToString(tone(SampleRate, nil).WAV())}
}

func TestDefaultsWithoutAnEngine(t *testing.T) {
	e := newEnv(t, nil, egress.Policy{})
	owner, ws := e.setup()
	rec := owner.do(http.MethodGet, route(ws, ""), nil)
	expect(t, rec, http.StatusOK)
	st := decode[statusReply](t, rec)
	// Local is the default even where the operator turned it off: dictation is then off,
	// and the reason says so, until an admin picks another engine.
	if st.Engine != "" || !strings.Contains(st.Reason, "local speech model is off") || st.ReasonCode != apierr.CodeDictationLocalOff ||
		st.Local != nil || st.MaxSeconds != 600 ||
		st.Settings.Engine != EngineLocal || st.Settings.API.APIKey != "not set" || st.Settings.API.BaseURL != defaultBaseURL {
		t.Fatalf("status = %+v", st)
	}
	rec = owner.do(http.MethodPost, route(ws, "transcriptions"), speech())
	expect(t, rec, http.StatusServiceUnavailable)
	// The settings form sends the engine back unchanged: saving it with another field works.
	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"engine": "local", "language": "pt"}), http.StatusOK)
	// But local can't be chosen anew when the operator turned it off, and auto is gone.
	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"engine": "api"}), http.StatusOK)
	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"engine": "local"}), http.StatusBadRequest)
	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"engine": "auto"}), http.StatusBadRequest)
}

func TestAPIEngine(t *testing.T) {
	e := newEnv(t, &fakeLocal{state: LocalState{State: LocalReady}, text: "local text"}, egress.Policy{})
	owner, ws := e.setup()
	alice := e.join(owner, ws, "alice")
	e.api.text = "I use task to in Zansibar"
	owner.putDoc(owner.privateSpace(ws), GlossaryFile, "- taskto\n")
	alice.putDoc(alice.privateSpace(ws), GlossaryFile, "- Zanzibar: a place\n")

	// Plain http to another host can't carry a key; this machine can.
	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"api": map[string]any{
		"base_url": "http://speech.example.com/v1", "api_key": fakeKey,
	}}), http.StatusBadRequest)
	rec := owner.do(http.MethodPatch, route(ws, ""), map[string]any{"language": "pt", "api": map[string]any{
		"base_url": e.api.URL + "/v1/", "model": "whisper-large-v3", "api_key": fakeKey,
	}})
	expect(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), fakeKey) {
		t.Fatal("the reply holds the key")
	}
	// A key alone doesn't send audio out: the engine stays local until an admin picks the API.
	st := decode[statusReply](t, rec)
	if st.Engine != EngineLocal || st.Settings.Engine != EngineLocal || st.Settings.API.APIKey != "set" ||
		st.Settings.API.BaseURL != e.api.URL+"/v1" || st.Settings.Language != "pt" {
		t.Fatalf("status = %+v", st)
	}
	rec = alice.do(http.MethodPost, route(ws, "transcriptions"), speech())
	expect(t, rec, http.StatusOK)
	if got := decode[transcribeReply](t, rec); got.Engine != EngineLocal || e.api.last != nil {
		t.Fatalf("with a key and no choice: reply = %+v, the API was called: %v", got, e.api.last != nil)
	}
	rec = owner.do(http.MethodPatch, route(ws, ""), map[string]any{"engine": "api"})
	expect(t, rec, http.StatusOK)
	st = decode[statusReply](t, rec)
	if st.Engine != EngineAPI || st.Settings.API.APIKey != "set" || len(st.Credits) != 0 {
		t.Fatalf("status = %+v", st)
	}
	var sealed []byte
	if err := e.db.QueryRow(`SELECT api_key FROM dictation_settings WHERE workspace_id = ?`, ws).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(fakeKey)) {
		t.Fatal("the key is stored in clear")
	}

	// Alice, a member, dictates: her glossary applies, the owner's private one doesn't.
	rec = alice.do(http.MethodPost, route(ws, "transcriptions"), speech())
	expect(t, rec, http.StatusOK)
	if got := decode[transcribeReply](t, rec); got.Text != "I use task to in Zanzibar" || got.Engine != EngineAPI {
		t.Fatalf("reply = %+v", got)
	}
	if got := e.api.last.Header.Get("Authorization"); got != "Bearer "+fakeKey {
		t.Fatalf("Authorization = %q", got)
	}
	if e.api.form["model"] != "whisper-large-v3" || e.api.form["language"] != "pt" || e.api.form["prompt"] != "" {
		t.Fatalf("form = %v", e.api.form)
	}
	if a, err := ParseWAV(e.api.wav); err != nil || len(a.Samples) != SampleRate {
		t.Fatalf("the API got %d samples, %v", len(a.Samples), err)
	}
	// The request's language wins over the workspace's.
	body := speech()
	body["language"] = "en"
	expect(t, alice.do(http.MethodPost, route(ws, "transcriptions"), body), http.StatusOK)
	if e.api.form["language"] != "en" {
		t.Fatalf("language = %q", e.api.form["language"])
	}

	// With no language set for the workspace, the user's own: their pick, else their
	// browser's; "" in the request still leaves it to the engine.
	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"language": ""}), http.StatusOK)
	language := func(body map[string]any) string {
		t.Helper()
		expect(t, alice.do(http.MethodPost, route(ws, "transcriptions"), body), http.StatusOK)
		return e.api.form["language"]
	}
	if got := language(speech()); got != "" {
		t.Errorf("nothing known: language = %q, want none", got)
	}
	body = speech()
	body["browser_locale"] = "es-419"
	if got := language(body); got != "es" {
		t.Errorf("the browser's: language = %q, want es", got)
	}
	expect(t, alice.do(http.MethodPatch, "/api/me", map[string]any{"locale": "pt-BR"}), http.StatusOK)
	if got := language(body); got != "pt" {
		t.Errorf("the user's pick: language = %q, want pt", got)
	}
	body["language"] = ""
	if got := language(body); got != "" {
		t.Errorf("asked to detect: language = %q", got)
	}
	body = speech()
	body["browser_locale"] = "pt_BR"
	expect(t, alice.do(http.MethodPost, route(ws, "transcriptions"), body), http.StatusBadRequest)
	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"language": "pt"}), http.StatusOK)

	// A refusal names the status, never the key the endpoint echoed.
	e.api.code = http.StatusUnauthorized
	rec = alice.do(http.MethodPost, route(ws, "transcriptions"), speech())
	expect(t, rec, http.StatusBadGateway)
	if strings.Contains(rec.Body.String(), fakeKey) {
		t.Fatal("the error holds the key")
	}
	e.api.code = http.StatusOK

	// The stored key doesn't follow the endpoint to another host unless it is sent again.
	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"api": map[string]any{
		"base_url": "https://speech.example.com/v1",
	}}), http.StatusBadRequest)
	// Nor to another port, or the same host in other letters with a trailing dot plus another port.
	for _, other := range []string{"http://127.0.0.1:9/v1", "https://LOCALHOST./v1"} {
		rec = owner.do(http.MethodPatch, route(ws, ""), map[string]any{"api": map[string]any{"base_url": other}})
		expect(t, rec, http.StatusBadRequest)
		if !strings.Contains(rec.Body.String(), "sent again") {
			t.Fatalf("%s: %s", other, rec.Body.String())
		}
	}
	if hostOf("HTTPS://Speech.Example.com./v1") != hostOf("https://speech.example.com:443/v2") {
		t.Fatal("the same host and port compare different")
	}
	// The same host, another path, keeps it.
	rec = owner.do(http.MethodPatch, route(ws, ""), map[string]any{"api": map[string]any{"base_url": e.api.URL + "/v1"}})
	expect(t, rec, http.StatusOK)
	// A redirect is the answer, not followed: the key never goes to where it points.
	elsewhere := newFakeAPI(t)
	e.api.redirect = elsewhere.URL + "/v1/audio/transcriptions"
	rec = alice.do(http.MethodPost, route(ws, "transcriptions"), speech())
	expect(t, rec, http.StatusBadGateway)
	if elsewhere.last != nil {
		t.Fatalf("the redirect was followed, with %q", elsewhere.last.Header.Get("Authorization"))
	}
	e.api.redirect = ""

	// Removing the key keeps the admin's choice: the api engine works without a key, for a
	// server on this machine.
	rec = owner.do(http.MethodPatch, route(ws, ""), map[string]any{"api": map[string]any{"api_key": nil}})
	expect(t, rec, http.StatusOK)
	if st := decode[statusReply](t, rec); st.Engine != EngineAPI || st.Settings.API.APIKey != "not set" {
		t.Fatalf("status = %+v", st)
	}
	expect(t, alice.do(http.MethodPost, route(ws, "transcriptions"), speech()), http.StatusOK)
	if got := e.api.last.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q without a key", got)
	}
}

func TestLocalEngineGetsTheGlossary(t *testing.T) {
	local := &fakeLocal{state: LocalState{State: LocalNotDownloaded}, text: "a trip to zansibar with task to"}
	e := newEnv(t, local, egress.Policy{})
	owner, ws := e.setup()
	alice := e.join(owner, ws, "alice")
	bob := e.join(owner, ws, "bob")
	alice.putDoc(alice.privateSpace(ws), GlossaryFile, "- Zanzibar\n- taskto\n")

	rec := bob.do(http.MethodGet, route(ws, ""), nil)
	expect(t, rec, http.StatusOK)
	if st := decode[statusReply](t, rec); st.Engine != EngineLocal || st.Local == nil || st.Local.Model != "fake-model" ||
		st.Local.State != LocalNotDownloaded || !strings.Contains(st.Reason, "downloads on first use") ||
		st.ReasonCode != apierr.CodeDictationLocalNotDownloaded {
		t.Fatalf("status = %+v", st)
	}
	// Not ready yet: 503 with the reason. Warm starts the download.
	rec = bob.do(http.MethodPost, route(ws, "transcriptions"), speech())
	expect(t, rec, http.StatusServiceUnavailable)
	expect(t, bob.do(http.MethodPost, route(ws, "warm"), nil), http.StatusOK)
	if local.warms != 1 {
		t.Fatalf("warms = %d", local.warms)
	}

	local.state = LocalState{State: LocalReady}
	// Bob can't read Alice's private space: her glossary isn't his.
	rec = bob.do(http.MethodPost, route(ws, "transcriptions"), speech())
	expect(t, rec, http.StatusOK)
	if got := decode[transcribeReply](t, rec).Text; got != "a trip to zansibar with task to" {
		t.Fatalf("bob's text = %q", got)
	}
	if terms := local.calls[len(local.calls)-1].Terms; len(terms) != 0 {
		t.Fatalf("bob's terms = %q", terms)
	}
	rec = alice.do(http.MethodPost, route(ws, "transcriptions"), speech())
	expect(t, rec, http.StatusOK)
	if got := decode[transcribeReply](t, rec).Text; got != "a trip to Zanzibar with taskto" {
		t.Fatalf("alice's text = %q", got)
	}
	if terms := local.calls[len(local.calls)-1].Terms; strings.Join(terms, ",") != "Zanzibar,taskto" {
		t.Fatalf("alice's terms = %q", terms)
	}
	// A failing engine is a 502 without details.
	local.err = fmt.Errorf("runtime crashed reading /secret/path")
	rec = alice.do(http.MethodPost, route(ws, "transcriptions"), speech())
	expect(t, rec, http.StatusBadGateway)
	if strings.Contains(rec.Body.String(), "/secret/path") {
		t.Fatal("the error leaks details")
	}
}

// Every route is the workspace's: another workspace's admin gets 404, a member can't change
// the settings, and someone logged out gets 401.
func TestDictationStaysInItsWorkspace(t *testing.T) {
	e := newEnv(t, &fakeLocal{state: LocalState{State: LocalReady}, text: "secret words"}, egress.Policy{})
	owner, ws := e.setup()
	member := e.join(owner, ws, "member")
	stranger, other := e.otherWorkspace(owner, ws)
	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"api": map[string]any{"api_key": fakeKey, "base_url": e.api.URL + "/v1"}}),
		http.StatusOK)

	for _, tc := range []struct {
		method, rest string
		body         any
	}{
		{http.MethodGet, "", nil},
		{http.MethodPatch, "", map[string]any{"engine": "local"}},
		{http.MethodPost, "warm", nil},
		{http.MethodPost, "transcriptions", speech()},
	} {
		rec := stranger.do(tc.method, route(ws, tc.rest), tc.body)
		expect(t, rec, http.StatusNotFound)
		if strings.Contains(rec.Body.String(), "secret words") {
			t.Fatal("a stranger got a transcription")
		}
		expect(t, (&client{e: e}).do(tc.method, route(ws, tc.rest), tc.body), http.StatusUnauthorized)
	}
	expect(t, member.do(http.MethodPatch, route(ws, ""), map[string]any{"engine": "local"}), http.StatusForbidden)
	expect(t, member.do(http.MethodPatch, route(ws, ""), map[string]any{"api": map[string]any{"api_key": nil}}), http.StatusForbidden)

	// The other workspace has its own settings: no key of ws's.
	rec := stranger.do(http.MethodGet, route(other, ""), nil)
	expect(t, rec, http.StatusOK)
	if st := decode[statusReply](t, rec); st.Settings.API.APIKey != "not set" || st.Engine != EngineLocal {
		t.Fatalf("the other workspace = %+v", st)
	}
	// A sealed key copied to the other workspace's row, with the API as its engine, doesn't open there.
	if _, err := e.db.Exec(`INSERT INTO dictation_settings (workspace_id, engine, api_key, api_base_url)
		SELECT ?, 'api', api_key, api_base_url FROM dictation_settings WHERE workspace_id = ?`, other, ws); err != nil {
		t.Fatal(err)
	}
	expect(t, stranger.do(http.MethodPost, route(other, "transcriptions"), speech()), http.StatusInternalServerError)
}

func TestSettingsValidation(t *testing.T) {
	e := newEnv(t, nil, egress.MustParse("public"))
	owner, ws := e.setup()
	for name, body := range map[string]any{
		"engine":          map[string]any{"engine": "whisper"},
		"language":        map[string]any{"language": "Portuguese"},
		"model":           map[string]any{"api": map[string]any{"model": ""}},
		"base_url":        map[string]any{"api": map[string]any{"base_url": "ftp://example.com"}},
		"credentials":     map[string]any{"api": map[string]any{"base_url": "https://someone@example.com/v1"}},
		"short key":       map[string]any{"api": map[string]any{"api_key": "short"}},
		"egress":          map[string]any{"api": map[string]any{"base_url": "https://speech.internal.example/v1"}},
		"unknown field":   map[string]any{"engines": "auto"},
		"unknown add-on":  map[string]any{"accept_notice": "desertant-voz"},
		"api not object":  map[string]any{"api": "x"},
		"language number": map[string]any{"language": 1},
	} {
		rec := owner.do(http.MethodPatch, route(ws, ""), body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, %s", name, rec.Code, rec.Body.String())
		}
	}
	rec := owner.do(http.MethodPatch, route(ws, ""), map[string]any{"api": map[string]any{"base_url": "https://speech.internal.example/v1"}})
	if !strings.Contains(rec.Body.String(), "egress policy") {
		t.Fatalf("egress refusal = %s", rec.Body.String())
	}
}

func TestTranscriptionValidation(t *testing.T) {
	e := newEnv(t, &fakeLocal{state: LocalState{State: LocalReady}, text: "ok"}, egress.Policy{})
	owner, ws := e.setup()
	stereo := tone(10, nil).WAV()
	stereo[22] = 2
	for name, tc := range map[string]struct {
		body any
		want string
	}{
		"not base64": {map[string]any{"audio_base64": "%%%"}, "base64"},
		"not wav":    {map[string]any{"audio_base64": base64.StdEncoding.EncodeToString([]byte("OggS....."))}, "WAV"},
		"stereo":     {map[string]any{"audio_base64": base64.StdEncoding.EncodeToString(stereo)}, "mono"},
		"empty":      {map[string]any{"audio_base64": base64.StdEncoding.EncodeToString(Audio{}.WAV())}, "empty"},
		"too long":   {map[string]any{"audio_base64": base64.StdEncoding.EncodeToString(tone(maxSamples+SampleRate, nil).WAV())}, "10 minutes"},
		"language":   {map[string]any{"audio_base64": speech()["audio_base64"], "language": "PT-br"}, "ISO 639-1"},
	} {
		rec := owner.do(http.MethodPost, route(ws, "transcriptions"), tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// Too many at once: 429, until a slot frees.
	for range maxInFlight {
		e.svc.busy <- struct{}{}
	}
	expect(t, owner.do(http.MethodPost, route(ws, "transcriptions"), speech()), http.StatusTooManyRequests)
	<-e.svc.busy
	expect(t, owner.do(http.MethodPost, route(ws, "transcriptions"), speech()), http.StatusOK)
	for range maxInFlight - 1 {
		<-e.svc.busy
	}
	// One at a time per person: another member still gets through.
	if !e.svc.claim(owner.id) {
		t.Fatal("no slot")
	}
	rec := owner.do(http.MethodPost, route(ws, "transcriptions"), speech())
	expect(t, rec, http.StatusTooManyRequests)
	if !strings.Contains(rec.Body.String(), "already dictating") {
		t.Fatalf("reply = %s", rec.Body.String())
	}
	other := e.join(owner, ws, "other")
	expect(t, other.do(http.MethodPost, route(ws, "transcriptions"), speech()), http.StatusOK)
	e.svc.release(owner.id)
	expect(t, owner.do(http.MethodPost, route(ws, "transcriptions"), speech()), http.StatusOK)

	// An engine that runs out of time is a 504.
	e.local.err = context.DeadlineExceeded
	expect(t, owner.do(http.MethodPost, route(ws, "transcriptions"), speech()), http.StatusGatewayTimeout)
	e.local.err = nil

	// Over the request limit: 413.
	big := strings.Repeat("A", maxAudioRequest)
	expect(t, owner.do(http.MethodPost, route(ws, "transcriptions"), map[string]any{"audio_base64": big}), http.StatusRequestEntityTooLarge)
}

// Two admins changing different fields at once both keep their change.
func TestConcurrentSettingsChanges(t *testing.T) {
	e := newEnv(t, nil, egress.Policy{})
	owner, ws := e.setup()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := map[string]any{"language": "pt"}
			if i%2 == 1 {
				body = map[string]any{"api": map[string]any{"model": "whisper-large-v3"}}
			}
			// A copy of the client per request: the recorder isn't shared.
			c := &client{e: e, id: owner.id, cookie: owner.cookie}
			if rec := c.do(http.MethodPatch, route(ws, ""), body); rec.Code != http.StatusOK {
				t.Errorf("status %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	st := decode[statusReply](t, owner.do(http.MethodGet, route(ws, ""), nil))
	if st.Settings.Language != "pt" || st.Settings.API.Model != "whisper-large-v3" {
		t.Fatalf("settings = %+v", st.Settings)
	}
}
