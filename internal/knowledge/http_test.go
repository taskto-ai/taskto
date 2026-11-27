package knowledge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/db"
)

const fakePassword = "fake-test-password"

type env struct {
	t       *testing.T
	h       http.Handler
	db      *sql.DB
	dataDir string
	store   *Store
	spaces  *access.Checker
}

var setupToken = auth.NewSetupToken()

func newEnv(t *testing.T, opts Options) *env {
	t.Helper()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	accounts := auth.New(sqlDB, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
	spaces := access.New(sqlDB, accounts)
	mux := http.NewServeMux()
	accounts.Routes(mux)
	spaces.Routes(mux)
	dataDir := t.TempDir()
	store := New(dataDir, opts)
	store.Routes(mux, spaces)
	return &env{t: t, h: auth.CSRF(mux, RawBodyRoutes...), db: sqlDB, dataDir: dataDir, store: store, spaces: spaces}
}

type user struct {
	e      *env
	id     int64
	cookie *http.Cookie
}

type rawBody []byte

func (u *user) do(method, target string, body any) *httptest.ResponseRecorder {
	u.e.t.Helper()
	var r *http.Request
	if raw, ok := body.(rawBody); ok {
		r = httptest.NewRequest(method, target, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/octet-stream")
	} else if body == nil {
		r = httptest.NewRequest(method, target, nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			u.e.t.Fatal(err)
		}
		r = httptest.NewRequest(method, target, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if u.cookie != nil {
		r.AddCookie(u.cookie)
	}
	rec := httptest.NewRecorder()
	u.e.h.ServeHTTP(rec, r)
	for _, ck := range rec.Result().Cookies() {
		if ck.MaxAge >= 0 {
			u.cookie = ck
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

func expectCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string, params map[string]any) {
	t.Helper()
	expect(t, rec, status)
	var b struct {
		Error  string
		Code   string
		Params map[string]any
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || b.Error == "" || b.Code != code || !reflect.DeepEqual(b.Params, params) {
		t.Fatalf("body %s, want code %q and params %v", rec.Body, code, params)
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

type world struct {
	e                                    *env
	ws                                   int64
	owner, alice, bob, mallory, stranger *user
	team, old                            access.Space
	ownerPrivate, alicePrivate           access.Space
	strangerPrivate                      access.Space
}

func newWorld(t *testing.T, opts Options) *world {
	t.Helper()
	e := newEnv(t, opts)
	w := &world{e: e}
	w.owner = &user{e: e}
	rec := w.owner.do(http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": fakePassword, "workspace_name": "Home", "setup_token": setupToken,
	})
	expect(t, rec, http.StatusCreated)
	me := decode[meReply](t, rec)
	w.owner.id, w.ws = me.User.ID, me.Workspaces[0].WorkspaceID

	w.alice = w.join("Alice")
	w.bob = w.join("Bob")
	w.mallory = w.join("Mallory")
	w.stranger = &user{e: e}

	w.team = w.createShared("Team")
	w.addMember(w.team, w.alice, access.RoleEditor)
	w.addMember(w.team, w.bob, access.RoleViewer)
	w.old = w.createShared("Old")
	expect(t, w.owner.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/spaces/%d", w.ws, w.old.ID),
		map[string]bool{"archived": true}), http.StatusOK)

	w.ownerPrivate = w.private(w.owner, w.ws)
	w.alicePrivate = w.private(w.alice, w.ws)

	w.stranger = w.join("Stranger")
	w.strangerPrivate = w.private(w.stranger, w.ws)
	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", w.ws, w.stranger.id), nil),
		http.StatusNoContent)
	return w
}

func (w *world) join(name string) *user {
	w.e.t.Helper()
	rec := w.owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", w.ws),
		map[string]string{"email": strings.ToLower(name) + "@example.com", "role": "member"})
	expect(w.e.t, rec, http.StatusCreated)
	token := decode[struct{ Token string }](w.e.t, rec).Token
	u := &user{e: w.e}
	rec = u.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token, "name": name, "password": fakePassword})
	expect(w.e.t, rec, http.StatusCreated)
	u.id = decode[meReply](w.e.t, rec).User.ID
	return u
}

func (w *world) createShared(name string) access.Space {
	w.e.t.Helper()
	rec := w.owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces", w.ws), map[string]string{"name": name})
	expect(w.e.t, rec, http.StatusCreated)
	return decode[access.Space](w.e.t, rec)
}

func (w *world) addMember(s access.Space, u *user, role access.Role) {
	w.e.t.Helper()
	rec := w.owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members", s.WorkspaceID, s.ID),
		map[string]any{"user_id": u.id, "role": role})
	expect(w.e.t, rec, http.StatusNoContent)
}

func (w *world) private(u *user, ws int64) access.Space {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces", ws), nil)
	expect(w.e.t, rec, http.StatusOK)
	for _, s := range decode[struct{ Spaces []access.Space }](w.e.t, rec).Spaces {
		if s.Kind == access.Private {
			return s
		}
	}
	w.e.t.Fatalf("user %d has no private space", u.id)
	return access.Space{}
}

func spacePath(s access.Space, rest string) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d%s", s.WorkspaceID, s.ID, rest)
}

type docJSON struct {
	Path          string
	Version       string
	Size          int64
	Content       *string
	ContentBase64 *string `json:"content_base64"`
}

func TestHTTPDocs(t *testing.T) {
	w := newWorld(t, Options{MaxFileSize: 1000})
	sp, alice := w.team, w.alice

	rec := alice.do(http.MethodPut, spacePath(sp, "/docs/notes/plan.md"), map[string]string{"content": "# Plan\n"})
	expect(t, rec, http.StatusCreated)
	created := decode[Result](t, rec)
	v1 := created.Doc.Version

	rec = w.bob.do(http.MethodGet, spacePath(sp, "/docs/notes/plan.md"), nil)
	expect(t, rec, http.StatusOK)
	if d := decode[docJSON](t, rec); d.Content == nil || *d.Content != "# Plan\n" || d.Version != v1 || d.ContentBase64 != nil {
		t.Fatalf("read = %s", rec.Body)
	}

	expect(t, w.owner.do(http.MethodPut, spacePath(sp, "/docs/notes/plan.md"),
		map[string]string{"content": "owner's", "base_version": v1}), http.StatusOK)
	rec = alice.do(http.MethodPut, spacePath(sp, "/docs/notes/plan.md"), map[string]string{"content": "alice's", "base_version": v1})
	expectCode(t, rec, http.StatusConflict, "doc_changed", map[string]any{"path": "notes/plan.md"})
	expectCode(t, alice.do(http.MethodPut, spacePath(sp, "/docs/notes/plan.md"), map[string]string{"content": "again"}),
		http.StatusConflict, "doc_exists", map[string]any{"path": "notes/plan.md"})
	expectCode(t, alice.do(http.MethodPut, spacePath(sp, "/docs/notes/plan.md/x.png"), map[string]string{"content_base64": "eA=="}),
		http.StatusConflict, "doc_path_taken", map[string]any{"path": "notes/plan.md/x.png"})
	expectCode(t, alice.do(http.MethodPut, spacePath(sp, "/docs/a%5Cb.md"), map[string]string{"content": "x"}),
		http.StatusBadRequest, "doc_name_invalid", map[string]any{"name": `a\b.md`})
	expectCode(t, alice.do(http.MethodPut, spacePath(sp, "/docs/c.md"), map[string]string{"content": "x", "message": "two\nlines"}),
		http.StatusBadRequest, "doc_message_invalid", map[string]any{"max": float64(MaxMessageLen)})

	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0}
	expect(t, alice.do(http.MethodPut, spacePath(sp, "/docs/img/logo.png"),
		map[string]string{"content_base64": base64.StdEncoding.EncodeToString(png)}), http.StatusCreated)
	rec = alice.do(http.MethodGet, spacePath(sp, "/docs/img/logo.png"), nil)
	expect(t, rec, http.StatusOK)
	if d := decode[docJSON](t, rec); d.ContentBase64 == nil || *d.ContentBase64 != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("read upload = %s", rec.Body)
	}
	rec = alice.do(http.MethodGet, spacePath(sp, "/docs/img/logo.png?raw=true"), nil)
	expect(t, rec, http.StatusOK)
	if !bytes.Equal(rec.Body.Bytes(), png) || rec.Header().Get("Content-Type") != "application/octet-stream" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename=logo.png` ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("download: headers %v, body %q", rec.Header(), rec.Body)
	}

	expect(t, alice.do(http.MethodPut, spacePath(sp, "/docs/x.md"), map[string]string{"content_base64": "eA=="}), http.StatusBadRequest)
	expect(t, alice.do(http.MethodPut, spacePath(sp, "/docs/x.png"), map[string]string{"content": "x"}), http.StatusBadRequest)
	expect(t, alice.do(http.MethodPut, spacePath(sp, "/docs/x.png"), map[string]string{"content_base64": "%%%"}), http.StatusBadRequest)

	expectCode(t, alice.do(http.MethodPut, spacePath(sp, "/docs/big.png"),
		map[string]string{"content_base64": base64.StdEncoding.EncodeToString(make([]byte, 1001))}),
		http.StatusRequestEntityTooLarge, "file_too_large", map[string]any{"path": "big.png", "max": float64(1000)})
	expect(t, alice.do(http.MethodPut, spacePath(sp, "/docs/big.md"),
		map[string]string{"content": strings.Repeat("x", 1001)}), http.StatusRequestEntityTooLarge)
	expectCode(t, alice.do(http.MethodPut, spacePath(sp, "/docs/huge.md"),
		map[string]string{"content": strings.Repeat("x", 1<<20)}), http.StatusRequestEntityTooLarge, "request_too_large", map[string]any{"max": float64(2*1000 + maxBody)})

	rec = alice.do(http.MethodGet, spacePath(sp, "/docs"), nil)
	expect(t, rec, http.StatusOK)
	if page := decode[Page](t, rec); len(page.Docs) != 2 || page.Docs[0].Path != "img/logo.png" || page.Docs[1].Path != "notes/plan.md" {
		t.Fatalf("list = %s", rec.Body)
	}
	rec = alice.do(http.MethodGet, spacePath(sp, "/docs?limit=1"), nil)
	expect(t, rec, http.StatusOK)
	if page := decode[Page](t, rec); len(page.Docs) != 1 || page.Next != "img/logo.png" {
		t.Fatalf("first page = %s", rec.Body)
	}
	rec = alice.do(http.MethodGet, spacePath(sp, "/docs?limit=1&after=img/logo.png"), nil)
	expect(t, rec, http.StatusOK)
	if page := decode[Page](t, rec); len(page.Docs) != 1 || page.Docs[0].Path != "notes/plan.md" || page.Next != "" {
		t.Fatalf("second page = %s", rec.Body)
	}
	expectCode(t, alice.do(http.MethodGet, spacePath(sp, "/docs?limit=0"), nil), http.StatusBadRequest, "bad_request", nil)
	expect(t, alice.do(http.MethodGet, spacePath(sp, "/docs?prefix=notes"), nil), http.StatusOK)

	rec = alice.do(http.MethodPatch, spacePath(sp, "/docs/notes/plan.md"), map[string]string{"to": "plan.md"})
	expect(t, rec, http.StatusOK)
	moved := decode[Result](t, rec)
	if moved.Doc.Path != "plan.md" {
		t.Fatalf("move = %s", rec.Body)
	}
	rec = alice.do(http.MethodGet, spacePath(sp, "/history/notes/plan.md"), nil)
	expect(t, rec, http.StatusOK)
	h := decode[History](t, rec)
	if len(h.Commits) != 3 || h.Commits[0].Change != "delete" || h.Commits[2].Commit != created.Commit ||
		h.Commits[2].Author != (Person{Name: "Alice", Email: "alice@example.com"}) || h.Commits[1].Author.Name != "Owner" {
		t.Fatalf("history = %s", rec.Body)
	}
	rec = alice.do(http.MethodGet, spacePath(sp, "/docs/notes/plan.md?at="+created.Commit), nil)
	expect(t, rec, http.StatusOK)
	if d := decode[docJSON](t, rec); *d.Content != "# Plan\n" {
		t.Fatalf("old version = %s", rec.Body)
	}
	expect(t, alice.do(http.MethodGet, spacePath(sp, "/docs/plan.md?at=nope"), nil), http.StatusBadRequest)
	rec = alice.do(http.MethodPost, spacePath(sp, "/restore/notes/plan.md"), map[string]string{"commit": created.Commit})
	expect(t, rec, http.StatusOK)
	if decode[Result](t, rec).Doc.Version != v1 {
		t.Fatalf("restore = %s", rec.Body)
	}
	expect(t, alice.do(http.MethodPost, spacePath(sp, "/restore/notes/plan.md"),
		map[string]string{"commit": created.Commit, "base_version": moved.Doc.Version}), http.StatusConflict)
	expect(t, alice.do(http.MethodDelete, spacePath(sp, "/docs/plan.md"), map[string]string{"base_version": v1}), http.StatusConflict)
	expect(t, alice.do(http.MethodDelete, spacePath(sp, "/docs/plan.md"), nil), http.StatusOK)
	expectCode(t, alice.do(http.MethodGet, spacePath(sp, "/docs/plan.md"), nil), http.StatusNotFound, "doc_not_found", nil)
	rec = alice.do(http.MethodGet, spacePath(sp, "/history"), nil)
	expect(t, rec, http.StatusOK)
	if h := decode[History](t, rec); len(h.Commits) != 6 {
		t.Fatalf("space history = %s", rec.Body)
	}
}

func TestHTTPAccess(t *testing.T) {
	w := newWorld(t, Options{})
	for _, sp := range []access.Space{w.team, w.ownerPrivate, w.alicePrivate} {
		u := w.owner
		if sp == w.alicePrivate {
			u = w.alice
		}
		expect(t, u.do(http.MethodPut, spacePath(sp, "/docs/a.md"), map[string]string{"content": "secret"}), http.StatusCreated)
		expect(t, u.do(http.MethodPut, spacePath(sp, "/docs/v.mp4"), rawBody("secret video")), http.StatusCreated)
	}
	commit := decode[History](t, w.alice.do(http.MethodGet, spacePath(w.alicePrivate, "/history"), nil)).Commits[0].Commit

	type route struct {
		method, path string
		body         any
		write        bool
	}
	routes := []route{
		{http.MethodGet, "/docs", nil, false},
		{http.MethodGet, "/docs/a.md", nil, false},
		{http.MethodGet, "/docs/a.md?raw=true", nil, false},
		{http.MethodGet, "/docs/v.mp4", nil, false},
		{http.MethodGet, "/docs/v.mp4?raw=true", nil, false},
		{http.MethodGet, "/history", nil, false},
		{http.MethodGet, "/history/a.md", nil, false},
		{http.MethodPut, "/docs/b.md", map[string]string{"content": "x"}, true},
		{http.MethodPut, "/docs/b.mp4", rawBody("x"), true},
		{http.MethodPut, "/docs/v.mp4", rawBody("overwritten"), true},
		{http.MethodPatch, "/docs/a.md", map[string]string{"to": "moved.md"}, true},
		{http.MethodDelete, "/docs/a.md", nil, true},
		{http.MethodPost, "/restore/a.md", map[string]string{"commit": commit}, true},
		{http.MethodDelete, "/history/a.md", nil, true},
	}
	tests := []struct {
		name        string
		u           *user
		sp          access.Space
		read, write int
	}{
		{"a member of no shared space", w.mallory, w.team, http.StatusNotFound, http.StatusNotFound},
		{"a viewer", w.bob, w.team, http.StatusOK, http.StatusForbidden},
		{"an admin in someone's private space", w.owner, w.alicePrivate, http.StatusNotFound, http.StatusNotFound},
		{"a member in someone's private space", w.bob, w.alicePrivate, http.StatusNotFound, http.StatusNotFound},
		{"a former member in their old private space", w.stranger, w.strangerPrivate, http.StatusNotFound, http.StatusNotFound},
		{"an editor of an archived space", w.owner, w.old, http.StatusOK, http.StatusForbidden},
		{"no session", &user{e: w.e}, w.team, http.StatusUnauthorized, http.StatusUnauthorized},
		{"a space addressed through another workspace", w.alice, access.Space{WorkspaceID: w.ws + 1, ID: w.alicePrivate.ID}, http.StatusNotFound, http.StatusNotFound},
		{"a space that doesn't exist", w.alice, access.Space{WorkspaceID: w.ws, ID: 999}, http.StatusNotFound, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, r := range routes {
				want := tt.read
				if r.write {
					want = tt.write
				}
				rec := tt.u.do(r.method, spacePath(tt.sp, r.path), r.body)
				if want == http.StatusOK && rec.Code == http.StatusNotFound && tt.sp == w.old {
					continue
				}
				if rec.Code != want {
					t.Errorf("%s %s: status %d, want %d; body %s", r.method, r.path, rec.Code, want, rec.Body)
				}
				if strings.Contains(rec.Body.String(), "secret") && tt.read != http.StatusOK {
					t.Errorf("%s %s: leaked the content", r.method, r.path)
				}
			}
		})
	}

	for _, sp := range []access.Space{w.team, w.alicePrivate} {
		u := w.owner
		if sp == w.alicePrivate {
			u = w.alice
		}
		rec := u.do(http.MethodGet, spacePath(sp, "/docs"), nil)
		if page := decode[Page](t, rec); len(page.Docs) != 2 || page.Docs[0].Path != "a.md" || page.Docs[1].Path != "v.mp4" {
			t.Errorf("space %d now lists %s", sp.ID, rec.Body)
		}
		rec = u.do(http.MethodGet, spacePath(sp, "/docs/v.mp4?raw=true"), nil)
		if rec.Body.String() != "secret video" {
			t.Errorf("space %d's video is now %q", sp.ID, rec.Body)
		}
	}
}

func TestHTTPUnsafeNames(t *testing.T) {
	w := newWorld(t, Options{})
	sp, u := w.team, w.alice
	expect(t, u.do(http.MethodPut, spacePath(sp, "/docs/a.md"), map[string]string{"content": "x"}), http.StatusCreated)
	commit := decode[History](t, u.do(http.MethodGet, spacePath(sp, "/history"), nil)).Commits[0].Commit
	gitConfig := filepath.Join(sp.Dir(w.e.dataDir), ".git", "config")
	before, err := os.ReadFile(gitConfig)
	if err != nil {
		t.Fatal(err)
	}

	refused := []string{
		".git", ".git/config", ".git/hooks/pre-commit", ".GIT/config", ".Git/HEAD", ".git./config",
		"git~1/config", ".g%E2%80%8Cit/config", "notes/.git/config", "a%5C..%5Cb", "a%00b",
	}
	cleaned := []string{"..%2F..%2Fsecret", "%2e%2e/%2e%2e/secret", "notes/../../../secret", "/etc/passwd"}

	try := func(name string) []*httptest.ResponseRecorder {
		return []*httptest.ResponseRecorder{
			u.do(http.MethodGet, spacePath(sp, "/docs/"+name), nil),
			u.do(http.MethodGet, spacePath(sp, "/docs/"+name+"?raw=true"), nil),
			u.do(http.MethodPut, spacePath(sp, "/docs/"+name), map[string]string{"content_base64": "cHduZWQ="}),
			u.do(http.MethodPatch, spacePath(sp, "/docs/"+name), map[string]string{"to": "b.md"}),
			u.do(http.MethodDelete, spacePath(sp, "/docs/"+name), nil),
			u.do(http.MethodGet, spacePath(sp, "/history/"+name), nil),
			u.do(http.MethodPost, spacePath(sp, "/restore/"+name), map[string]string{"commit": commit}),
			u.do(http.MethodGet, spacePath(sp, "/docs?prefix="+name), nil),
			u.do(http.MethodPatch, spacePath(sp, "/docs/a.md"), map[string]string{"to": decodeURL(t, name)}),
		}
	}
	for _, name := range refused {
		for i, rec := range try(name) {
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s, request %d: status %d, want 400; body %s", name, i, rec.Code, rec.Body)
			}
		}
	}
	for _, name := range cleaned {
		for i, rec := range try(name) {
			if rec.Code < 300 {
				t.Errorf("%s, request %d: status %d; body %s", name, i, rec.Code, rec.Body)
			}
		}
	}

	after, err := os.ReadFile(gitConfig)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf(".git/config changed: %q, %v", after, err)
	}
	rec := u.do(http.MethodGet, spacePath(sp, "/docs"), nil)
	if page := decode[Page](t, rec); len(page.Docs) != 1 || page.Docs[0].Path != "a.md" {
		t.Fatalf("the space now lists %s", rec.Body)
	}
	if _, err := os.Stat(filepath.Join(w.e.dataDir, "secret")); err == nil {
		t.Fatal("a file was written outside the space")
	}
}

func decodeURL(t *testing.T, s string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/"+s, nil)
	return strings.TrimPrefix(r.URL.Path, "/")
}

func TestHTTPNonUTF8MarkdownReadsBack(t *testing.T) {
	w := newWorld(t, Options{})
	sp, alice := w.team, w.alice
	raw := []byte{0xff, 0xfe, 'x', 0x80}
	expect(t, alice.do(http.MethodPut, spacePath(sp, "/docs/blob.bin"),
		map[string]string{"content_base64": base64.StdEncoding.EncodeToString(raw)}), http.StatusCreated)
	expect(t, alice.do(http.MethodPatch, spacePath(sp, "/docs/blob.bin"), map[string]string{"to": "blob.md"}), http.StatusOK)
	rec := alice.do(http.MethodGet, spacePath(sp, "/docs/blob.md"), nil)
	expect(t, rec, http.StatusOK)
	if d := decode[docJSON](t, rec); d.Content != nil || d.ContentBase64 == nil || *d.ContentBase64 != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("read = %s", rec.Body)
	}
}

func TestHTTPDeleteWithEmptyChunkedBody(t *testing.T) {
	w := newWorld(t, Options{})
	sp, alice := w.team, w.alice
	expect(t, alice.do(http.MethodPut, spacePath(sp, "/docs/gone.md"), map[string]string{"content": "x"}), http.StatusCreated)
	r := httptest.NewRequest(http.MethodDelete, spacePath(sp, "/docs/gone.md"), strings.NewReader(""))
	r.ContentLength = -1
	r.TransferEncoding = []string{"chunked"}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.AddCookie(alice.cookie)
	rec := httptest.NewRecorder()
	alice.e.h.ServeHTTP(rec, r)
	expect(t, rec, http.StatusOK)
	expect(t, alice.do(http.MethodGet, spacePath(sp, "/docs/gone.md"), nil), http.StatusNotFound)
}

func TestHTTPWithoutRequireSpace(t *testing.T) {
	w := newWorld(t, Options{})
	s := w.e.store
	const prefix = "/api/workspaces/{workspace}/spaces/{space}"
	routes := []struct {
		pattern string
		h       http.HandlerFunc
		method  string
		path    string
		body    string
	}{
		{"GET " + prefix + "/docs", s.handleList, http.MethodGet, "/docs", ""},
		{"GET " + prefix + "/docs/{path...}", s.handleRead, http.MethodGet, "/docs/a.md", ""},
		{"PUT " + prefix + "/docs/{path...}", s.handlePut, http.MethodPut, "/docs/a.md", `{"content":"x"}`},
		{"PATCH " + prefix + "/docs/{path...}", s.handleMove, http.MethodPatch, "/docs/a.md", `{"to":"b.md"}`},
		{"DELETE " + prefix + "/docs/{path...}", s.handleDelete, http.MethodDelete, "/docs/a.md", ""},
		{"GET " + prefix + "/history", s.handleHistory, http.MethodGet, "/history", ""},
		{"POST " + prefix + "/restore/{path...}", s.handleRestore, http.MethodPost, "/restore/a.md", `{"commit":"0"}`},
	}
	bare := http.NewServeMux()
	for _, rt := range routes {
		bare.Handle(rt.pattern, rt.h)
	}
	send := func(h http.Handler, method, target, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(w.alice.cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	for _, rt := range routes {
		rec := send(bare, rt.method, spacePath(w.team, rt.path), rt.body)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s without RequireSpace: status = %d, want 500; body %s", rt.pattern, rec.Code, rec.Body)
		}
	}
	if _, err := os.Stat(access.SpaceDir(w.e.dataDir, 0, 0)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the zero space's directory exists, or can't be checked (err %v)", err)
	}

	expect(t, w.alice.do(http.MethodPut, spacePath(w.team, "/docs/a.md"), map[string]string{"content": "kept"}), http.StatusCreated)
	history := w.alice.do(http.MethodGet, spacePath(w.team, "/history"), nil).Body.String()
	mux := http.NewServeMux()
	for _, rt := range routes {
		if rt.method == http.MethodGet {
			continue
		}
		mux.Handle(rt.pattern, w.e.spaces.RequireSpace(access.Write, http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			rt.h(rw, r.WithContext(spaceOnly{r.Context()}))
		})))
	}
	for _, rt := range routes {
		if rt.method == http.MethodGet {
			continue
		}
		if rec := send(mux, rt.method, spacePath(w.team, rt.path), rt.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a user: status = %d, want 401; body %s", rt.pattern, rec.Code, rec.Body)
		}
	}
	rec := w.alice.do(http.MethodGet, spacePath(w.team, "/docs/a.md"), nil)
	expect(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "kept") {
		t.Fatalf("a.md changed: %s", rec.Body)
	}
	if got := w.alice.do(http.MethodGet, spacePath(w.team, "/history"), nil).Body.String(); got != history {
		t.Fatalf("history changed:\n%s\nwas\n%s", got, history)
	}
}

type spaceOnly struct{ context.Context }

func (c spaceOnly) Value(key any) any {
	if sp, ok := c.Context.Value(key).(access.Space); ok {
		return sp
	}
	return nil
}

func TestHTTPUnversioned(t *testing.T) {
	w := newWorld(t, Options{LargeFileSize: 100, MaxFileSize: 1000})
	sp, alice := w.team, w.alice

	video := []byte("0123456789 frames")
	rec := alice.do(http.MethodPut, spacePath(sp, "/docs/talk.mp4?message=Add+the+talk"), rawBody(video))
	expect(t, rec, http.StatusCreated)
	created := decode[Result](t, rec)
	if created.Doc.Versioned || created.Doc.Size != int64(len(video)) {
		t.Fatalf("created = %s", rec.Body)
	}
	expect(t, alice.do(http.MethodPut, spacePath(sp, "/docs/talk.mp4"), rawBody("again")), http.StatusConflict)
	expect(t, alice.do(http.MethodPut, spacePath(sp, "/docs/talk.mp4?base=x"), rawBody("again")), http.StatusBadRequest)
	expect(t, alice.do(http.MethodPut, spacePath(sp, "/docs/huge.bin"), rawBody(make([]byte, 1001))), http.StatusRequestEntityTooLarge)
	expect(t, w.bob.do(http.MethodPut, spacePath(sp, "/docs/bob.mp4"), rawBody("x")), http.StatusForbidden)
	rec = alice.do(http.MethodPut, spacePath(sp, "/docs/notes.md"), rawBody("# Notes\n"))
	expect(t, rec, http.StatusCreated)
	if !decode[Result](t, rec).Doc.Versioned {
		t.Fatalf("notes.md = %s", rec.Body)
	}
	rec = alice.do(http.MethodPut, spacePath(sp, "/docs/big.bin"), map[string]string{"content_base64": base64.StdEncoding.EncodeToString(make([]byte, 150))})
	expect(t, rec, http.StatusCreated)
	if decode[Result](t, rec).Doc.Versioned {
		t.Fatalf("big.bin = %s", rec.Body)
	}

	rec = w.bob.do(http.MethodGet, spacePath(sp, "/docs/talk.mp4"), nil)
	expect(t, rec, http.StatusOK)
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got["versioned"] != false ||
		got["content"] != nil || got["content_base64"] != nil || got["size"] != float64(len(video)) {
		t.Fatalf("read = %s", rec.Body)
	}
	rec = w.bob.do(http.MethodGet, spacePath(sp, "/docs"), nil)
	if page := decode[Page](t, rec); len(page.Docs) != 3 || page.Docs[1].Path != "notes.md" || !page.Docs[1].Versioned || page.Docs[2].Versioned {
		t.Fatalf("list = %s", rec.Body)
	}

	rec = w.bob.do(http.MethodGet, spacePath(sp, "/docs/talk.mp4?raw=true"), nil)
	expect(t, rec, http.StatusOK)
	if !bytes.Equal(rec.Body.Bytes(), video) || rec.Header().Get("Content-Type") != "video/mp4" ||
		rec.Header().Get("Content-Disposition") != "attachment; filename=talk.mp4" ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("download: headers %v, body %q", rec.Header(), rec.Body)
	}
	r := httptest.NewRequest(http.MethodGet, spacePath(sp, "/docs/talk.mp4?raw=true"), nil)
	r.Header.Set("Range", "bytes=2-5")
	r.AddCookie(w.bob.cookie)
	ranged := httptest.NewRecorder()
	w.e.h.ServeHTTP(ranged, r)
	if ranged.Code != http.StatusPartialContent || ranged.Body.String() != "2345" {
		t.Fatalf("range: %d %q", ranged.Code, ranged.Body)
	}

	rec = alice.do(http.MethodPut, spacePath(sp, "/docs/talk.mp4?base_version="+created.Doc.Version), rawBody("new cut"))
	expect(t, rec, http.StatusOK)
	expect(t, alice.do(http.MethodGet, spacePath(sp, "/docs/talk.mp4?raw=true&at="+created.Commit), nil), http.StatusGone)
	expect(t, alice.do(http.MethodPost, spacePath(sp, "/restore/talk.mp4"), map[string]string{"commit": created.Commit}), http.StatusGone)
	rec = alice.do(http.MethodGet, spacePath(sp, "/docs/talk.mp4?raw=true"), nil)
	if rec.Body.String() != "new cut" {
		t.Fatalf("current = %q", rec.Body)
	}

	expect(t, alice.do(http.MethodPatch, spacePath(sp, "/docs/talk.mp4"), rawBody(`{"to":"x.mp4"}`)), http.StatusUnsupportedMediaType)
}

func TestHTTPRemoveOldVersions(t *testing.T) {
	w := newWorld(t, Options{})
	put := func(u *user, content, base string) Entry {
		t.Helper()
		body := map[string]string{"content": content}
		status := http.StatusCreated
		if base != "" {
			body["base_version"], status = base, http.StatusOK
		}
		rec := u.do(http.MethodPut, spacePath(w.team, "/docs/notes.md"), body)
		expect(t, rec, status)
		return *decode[Result](t, rec).Doc
	}
	v := put(w.alice, "private", "")
	v = put(w.alice, "public", v.Version)
	target := spacePath(w.team, "/history/notes.md")
	history := func() []Commit {
		return decode[History](t, w.alice.do(http.MethodGet, target, nil)).Commits
	}

	res, err := w.e.db.Exec(`INSERT INTO workspaces (name) VALUES ('Elsewhere')`)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := res.LastInsertId()
	if _, err := w.e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, other, w.stranger.id); err != nil {
		t.Fatal(err)
	}
	rec := w.owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/tokens", w.ws), map[string]string{"name": "script"})
	expect(t, rec, http.StatusCreated)
	token := decode[struct{ Token string }](t, rec).Token

	for _, tt := range []struct {
		name   string
		u      *user
		target string
		status int
		code   string
	}{
		{"an editor who isn't an admin", w.alice, target, http.StatusForbidden, "admin_only"},
		{"a viewer", w.bob, target, http.StatusForbidden, "space_read_only"},
		{"a member of no shared space", w.mallory, target, http.StatusNotFound, "space_not_found"},
		{"another workspace's admin", w.stranger, target, http.StatusNotFound, "workspace_not_found"},
		{"another workspace's admin, through it", w.stranger,
			fmt.Sprintf("/api/workspaces/%d/spaces/%d/history/notes.md", other, w.team.ID), http.StatusNotFound, "space_not_found"},
		{"no session", &user{e: w.e}, target, http.StatusUnauthorized, "not_logged_in"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			expectCode(t, tt.u.do(http.MethodDelete, tt.target, nil), tt.status, tt.code, nil)
		})
	}
	t.Run("an admin's access token", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodDelete, target, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		w.e.h.ServeHTTP(rec, r)
		expect(t, rec, http.StatusForbidden)
	})
	if h := history(); len(h) != 2 || !*h[1].Versioned {
		t.Fatalf("history after the refusals = %+v", h)
	}

	expectCode(t, w.owner.do(http.MethodDelete, target, map[string]string{"base_version": h0(history())}), http.StatusConflict, "doc_changed", map[string]any{"path": "notes.md"})
	rec = w.owner.do(http.MethodDelete, target, map[string]string{"base_version": v.Version})
	expect(t, rec, http.StatusOK)
	got := decode[Removed](t, rec)
	if got.Removed != 1 || got.Doc == nil || got.Doc.Version != v.Version {
		t.Fatalf("removed = %s", rec.Body)
	}
	h := history()
	if len(h) != 2 || *h[1].Versioned || !*h[0].Versioned {
		t.Fatalf("history = %+v", h)
	}
	rec = w.owner.do(http.MethodGet, spacePath(w.team, "/docs/notes.md?raw=true&at="+h[1].Commit), nil)
	expectCode(t, rec, http.StatusGone, "doc_not_kept", nil)
	if strings.Contains(rec.Body.String(), "private") {
		t.Fatal("the removed version leaked")
	}
	expectCode(t, w.owner.do(http.MethodDelete, spacePath(w.team, "/history/never.md"), nil), http.StatusNotFound, "doc_not_found", nil)
}

func h0(h []Commit) string { return h[len(h)-1].Version }
