package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/audit/audittest"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/providers"
	"taskto.ai/taskto/internal/search"
	"taskto.ai/taskto/internal/serverkey"
)

var webApp = fstest.MapFS{
	"index.html":         {Data: []byte("<!doctype html><title>web app</title>")},
	"assets/main-abc.js": {Data: []byte("console.log(1)")},
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

func newHandler(t *testing.T, sqlDB *sql.DB, opts Options) http.Handler {
	t.Helper()
	if opts.WebUI == nil {
		opts.WebUI = webApp
	}
	h, err := New("test", sqlDB, opts)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func healthBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

func TestHealthz(t *testing.T) {
	rec := get(newHandler(t, openDB(t), Options{}), "/healthz")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := healthBody(t, rec)
	if body["status"] != "ok" || body["version"] != "test" || body["database"] != "ok" {
		t.Fatalf("body = %v, want status ok, version test and database ok", body)
	}
}

func TestHealthzWithTheDatabaseDown(t *testing.T) {
	sqlDB := openDB(t)
	h := newHandler(t, sqlDB, Options{})
	_ = sqlDB.Close()

	rec := get(h, "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	body := healthBody(t, rec)
	if body["status"] != "unavailable" || body["version"] != "test" || body["database"] != "unreachable" {
		t.Fatalf("body = %v, want status unavailable, version test and database unreachable", body)
	}
}

// The agent runtime's state is reported, and never changes the status code: the server
// works without it.
func TestHealthzReportsTheAgentRuntime(t *testing.T) {
	sqlDB := openDB(t)
	if body := healthBody(t, get(newHandler(t, sqlDB, Options{}), "/healthz")); body["agent"] != "off" {
		t.Fatalf("agent = %q with no agent runtime, want off", body["agent"])
	}
	for _, state := range []string{"starting", "ready", "stopped"} {
		rec := get(newHandler(t, sqlDB, Options{AgentState: func() string { return state }}), "/healthz")
		if body := healthBody(t, rec); rec.Code != http.StatusOK || body["agent"] != state || body["status"] != "ok" {
			t.Fatalf("healthz with the agent runtime %s = %d %v, want 200, status ok and agent %s", state, rec.Code, body, state)
		}
	}
}

func TestUnknownAPIRouteIs404(t *testing.T) {
	h := newHandler(t, openDB(t), Options{})
	for _, p := range []string{"/api", "/api/", "/api/nope", "/api/apps/expenses/surface"} {
		if rec := get(h, p); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
}

func TestWebAppAndSPAFallback(t *testing.T) {
	h := newHandler(t, openDB(t), Options{})
	for _, p := range []string{"/", "/boards/7", "/apis", "/healthz/more"} {
		rec := get(h, p)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>web app</title>") {
			t.Errorf("GET %s = %d %q, want the web app's index.html", p, rec.Code, rec.Body.String())
		}
	}
	rec := get(h, "/assets/main-abc.js")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("GET asset = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestEmbeddedWebAppByDefault(t *testing.T) {
	h, err := New("test", openDB(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The placeholder or the built app, depending on whether `make build` copied it in.
	if rec := get(h, "/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "taskto.ai") {
		t.Fatalf("GET / = %d %q", rec.Code, rec.Body.String())
	}
}

func TestAccountSpaceRoutesAndCSRFAreMounted(t *testing.T) {
	sqlDB := openDB(t)
	if _, err := db.Migrate(context.Background(), sqlDB); err != nil {
		t.Fatal(err)
	}
	h := newHandler(t, sqlDB, Options{})

	rec := get(h, "/api/setup")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"needed":true`) {
		t.Fatalf("GET /api/setup = %d %s", rec.Code, rec.Body.String())
	}
	if rec := get(h, "/api/me"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/me = %d, want 401", rec.Code)
	}
	for _, p := range []string{"/api/workspaces/1/spaces", "/api/workspaces/1/spaces/1/boards", "/api/workspaces/1/spaces/1/apps", "/api/workspaces/1/spaces/1/apps/1/surface"} {
		if rec := get(h, p); rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s = %d, want 401", p, rec.Code)
		}
	}

	// The CSRF guard wraps the whole server, so every /api route has it: the account and
	// space routes, the apps, and routes later cards add.
	for _, p := range []string{"/api/setup", "/api/login", "/api/workspaces/1/spaces", "/api/workspaces/1/spaces/1/boards", "/api/workspaces/1/spaces/1/apps", "/api/workspaces/1/spaces/1/apps/1/actions", "/api/not-yet-written"} {
		r := httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-site POST %s = %d, want 403", p, rec.Code)
		}
	}
}

func TestKnowledgeRoutesNeedADataDirectory(t *testing.T) {
	sqlDB := openDB(t)
	if _, err := db.Migrate(context.Background(), sqlDB); err != nil {
		t.Fatal(err)
	}
	const docs = "/api/workspaces/1/spaces/1/docs"
	if rec := get(newHandler(t, sqlDB, Options{}), docs); rec.Code != http.StatusNotFound {
		t.Fatalf("without a data directory, GET %s = %d, want 404", docs, rec.Code)
	}
	if rec := get(newHandler(t, sqlDB, Options{DataDir: t.TempDir()}), docs); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET %s = %d, want 401", docs, rec.Code)
	}
}

func TestSearchServesItsKnowledgeBase(t *testing.T) {
	sqlDB := openDB(t)
	if _, err := db.Migrate(context.Background(), sqlDB); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key, err := serverkey.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx := search.New(sqlDB, knowledge.New(dir, knowledge.Options{}), providers.NewStore(sqlDB, key), search.Options{})
	h := newHandler(t, sqlDB, Options{Search: idx})
	for _, p := range []string{"/api/workspaces/1/spaces/1/docs", "/api/workspaces/1/spaces/1/search?q=x", "/api/workspaces/1/search?q=x", "/api/workspaces/1/search/settings"} {
		if rec := get(h, p); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401", p, rec.Code)
		}
	}
}

// TestAuditEventsCarryTheClientAddress runs a login through the whole server: the event holds the
// address the request came from, and no one's password.
func TestAuditEventsCarryTheClientAddress(t *testing.T) {
	sqlDB := openDB(t)
	if _, err := db.Migrate(context.Background(), sqlDB); err != nil {
		t.Fatal(err)
	}
	const token = "fake-setup-token-for-the-audit-test"
	h := newHandler(t, sqlDB, Options{Auth: auth.Options{SetupToken: token, HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}}})
	post := func(path string, body map[string]string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.RemoteAddr = "198.51.100.23:4444"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if rec := post("/api/setup", map[string]string{"email": "owner@example.com", "name": "Owner", "password": fakePassword,
		"workspace_name": "Home", "setup_token": token}); rec.Code != http.StatusCreated {
		t.Fatalf("setup = %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("/api/login", map[string]string{"email": "owner@example.com", "password": "fake-wrong-password"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("login = %d", rec.Code)
	}
	rows, err := audit.List(context.Background(), sqlDB, audit.Filter{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("events = %+v, %v", rows, err)
	}
	for _, r := range rows {
		if r.ClientAddr == nil || *r.ClientAddr != "198.51.100.23" {
			t.Errorf("%s: client address %v, want 198.51.100.23", r.Action, r.ClientAddr)
		}
	}
	audittest.NoSecrets(t, sqlDB, map[string]string{"setup token": token, "password": fakePassword, "wrong password": "fake-wrong-password"})
}
