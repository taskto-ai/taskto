package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/providers"
	"taskto.ai/taskto/internal/serverkey"
)

var ctx = context.Background()

// Test passwords and keys: obviously fake.
const (
	fakePassword = "fake-test-password"
	fakeKey      = "fake-embeddings-key"
)

// setupToken is the first-run setup token the server would print to its log.
var setupToken = auth.NewSetupToken()

type env struct {
	t       testing.TB
	h       http.Handler
	idx     *Index
	kb      *knowledge.Store
	dataDir string
	embed   *fakeEmbeddings
}

// newEnv serves the account, space, provider, knowledge-base and search routes, as the
// server does, with a fake embeddings endpoint. The index's background work isn't
// started: searches sync lazily, and tests embed with embedPending.
func newEnv(t testing.TB) *env {
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
	store := providers.NewStore(sqlDB, key)
	mux := http.NewServeMux()
	accounts.Routes(mux)
	spaces.Routes(mux)
	providers.New(store, nil).Routes(mux, accounts)
	kb := knowledge.New(dir, knowledge.Options{})
	idx := New(sqlDB, kb, store, Options{})
	kb.Routes(mux, spaces)
	idx.Routes(mux, accounts, spaces)
	fake := newFakeEmbeddings(t)
	return &env{t: t, h: auth.CSRF(mux, knowledge.RawBodyRoutes...), idx: idx, kb: kb, dataDir: dir, embed: fake}
}

// user is a logged-in browser.
type user struct {
	e      *env
	id     int64
	name   string
	cookie *http.Cookie
}

func (u *user) do(method, target string, body any) *httptest.ResponseRecorder {
	u.e.t.Helper()
	var r *http.Request
	if body == nil {
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

func expect(t testing.TB, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
}

// expectCode checks an error's status, code and params (nil for none).
func expectCode(t testing.TB, rec *httptest.ResponseRecorder, status int, code string, params map[string]any) {
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

func decode[T any](t testing.TB, rec *httptest.ResponseRecorder) T {
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

// world is a workspace with people in every relation to its spaces:
//
//   - owner: admin, editor of team, secret and old (archived);
//   - alice: editor of team, viewer of old;
//   - bob: viewer of team;
//   - mallory: a member of the workspace, of no shared space;
//   - stranger: was a member, and left: their private space is closed to everyone.
type world struct {
	e                                    *env
	ws                                   int64
	owner, alice, bob, mallory, stranger *user
	team, secret, old                    access.Space
	private                              map[*user]access.Space
}

func newWorld(t testing.TB) *world {
	t.Helper()
	e := newEnv(t)
	w := &world{e: e, private: map[*user]access.Space{}}
	w.owner = &user{e: e, name: "Owner"}
	rec := w.owner.do(http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": fakePassword, "workspace_name": "Home", "setup_token": setupToken,
	})
	expect(t, rec, http.StatusCreated)
	me := decode[meReply](t, rec)
	w.owner.id, w.ws = me.User.ID, me.Workspaces[0].WorkspaceID

	w.alice = w.join("Alice")
	w.bob = w.join("Bob")
	w.mallory = w.join("Mallory")
	w.stranger = w.join("Stranger")

	w.team = w.createShared("Team")
	w.addMember(w.team, w.alice, access.RoleEditor)
	w.addMember(w.team, w.bob, access.RoleViewer)
	w.secret = w.createShared("Secret")
	w.old = w.createShared("Old")
	w.addMember(w.old, w.alice, access.RoleViewer)

	for _, u := range []*user{w.owner, w.alice, w.bob, w.mallory, w.stranger} {
		w.private[u] = w.privateOf(u)
	}
	return w
}

// finish archives old and has the stranger leave, once the tests have written to their
// spaces.
func (w *world) finish() {
	w.e.t.Helper()
	expect(w.e.t, w.owner.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/spaces/%d", w.ws, w.old.ID),
		map[string]bool{"archived": true}), http.StatusOK)
	expect(w.e.t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", w.ws, w.stranger.id), nil),
		http.StatusNoContent)
}

func (w *world) join(name string) *user {
	w.e.t.Helper()
	rec := w.owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", w.ws),
		map[string]string{"email": strings.ToLower(name) + "@example.com", "role": "member"})
	expect(w.e.t, rec, http.StatusCreated)
	token := decode[struct{ Token string }](w.e.t, rec).Token
	u := &user{e: w.e, name: name}
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

func (w *world) privateOf(u *user) access.Space {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces", w.ws), nil)
	expect(w.e.t, rec, http.StatusOK)
	for _, s := range decode[struct{ Spaces []access.Space }](w.e.t, rec).Spaces {
		if s.Kind == access.Private {
			return s
		}
	}
	w.e.t.Fatalf("user %d has no private space", u.id)
	return access.Space{}
}

// put writes a document as u, through the API.
func (w *world) put(u *user, sp access.Space, name, content string) {
	w.e.t.Helper()
	rec := u.do(http.MethodPut, fmt.Sprintf("/api/workspaces/%d/spaces/%d/docs/%s", sp.WorkspaceID, sp.ID, name),
		map[string]string{"content": content})
	expect(w.e.t, rec, http.StatusCreated)
}

// search searches the workspace, or one space, as u.
func (w *world) search(u *user, sp *access.Space, q string) *httptest.ResponseRecorder {
	w.e.t.Helper()
	target := fmt.Sprintf("/api/workspaces/%d/search?q=%s", w.ws, url.QueryEscape(q))
	if sp != nil {
		target = fmt.Sprintf("/api/workspaces/%d/spaces/%d/search?q=%s", sp.WorkspaceID, sp.ID, url.QueryEscape(q))
	}
	return u.do(http.MethodGet, target, nil)
}

// addAccount creates a workspace provider account on the fake embeddings endpoint and
// returns its ID.
func (w *world) addAccount(name string) int64 {
	w.e.t.Helper()
	rec := w.owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/providers", w.ws), map[string]any{
		"name": name, "base_url": w.e.embed.srv.URL + "/v1", "api_key": fakeKey,
		"models": []map[string]string{{"id": "chat"}},
	})
	expect(w.e.t, rec, http.StatusCreated)
	return decode[providers.Account](w.e.t, rec).ID
}

// setModel makes the fake endpoint the workspace's embeddings model, and consents to it
// for the team space, as its editor the owner.
func (w *world) setModel() int64 {
	w.e.t.Helper()
	account := w.addAccount("embed")
	rec := w.owner.do(http.MethodPut, fmt.Sprintf("/api/workspaces/%d/search/settings", w.ws),
		map[string]any{"embeddings": map[string]any{"account": account, "model": fakeModel}})
	expect(w.e.t, rec, http.StatusOK)
	w.consent(w.owner, w.team)
	return account
}

// consent consents, as u, to the workspace's embeddings model for sp.
func (w *world) consent(u *user, sp access.Space) {
	w.e.t.Helper()
	expect(w.e.t, u.do(http.MethodPut, embeddingURL(sp), map[string]bool{"enabled": true}), http.StatusOK)
}

// embedAll runs the embeddings worker until nothing is waiting; a worker that never gets
// there fails the test.
func (e *env) embedAll() {
	e.t.Helper()
	for range 100 {
		n, err := e.idx.embedPending(ctx)
		if err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	e.t.Fatal("documents are still waiting after 100 passes of the embeddings worker")
}

const (
	fakeModel = "fake-embed"
	fakeDims  = 64
)

// fakeEmbeddings is an OpenAI-compatible /embeddings endpoint whose vectors count words
// by meaning: the words of each concept below share a dimension, so "car" is near a text
// about an automobile that full-text search can't find; other words get a dimension by
// their hash.
type fakeEmbeddings struct {
	srv   *httptest.Server
	calls atomic.Int64
	// fail, while set, answers 503; block, while not nil, holds every request until closed.
	fail  atomic.Bool
	mu    sync.Mutex
	block chan struct{}
	// inputs is every text it was sent.
	inputs []string
}

var concepts = map[string]int{
	"car": 0, "automobile": 0, "vehicle": 0, "sedan": 0,
	"ocean": 1, "sea": 1, "marine": 1, "maritime": 1,
}

func newFakeEmbeddings(t testing.TB) *fakeEmbeddings {
	f := &fakeEmbeddings{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.mu.Lock()
		block := f.block
		f.mu.Unlock()
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer "+fakeKey {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		if f.fail.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != fakeModel {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.inputs = append(f.inputs, req.Input...)
		f.mu.Unlock()
		type item struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		}
		var data []item
		// In reverse, so the client has to order them by index.
		for i := len(req.Input) - 1; i >= 0; i-- {
			data = append(data, item{Index: i, Embedding: fakeVector(req.Input[i])})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// sent reports whether any text the endpoint was sent contains s.
func (f *fakeEmbeddings) sent(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.inputs, func(in string) bool { return strings.Contains(in, s) })
}

func (f *fakeEmbeddings) hold() (release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block = make(chan struct{})
	block := f.block
	return func() {
		f.mu.Lock()
		f.block = nil
		f.mu.Unlock()
		close(block)
	}
}

func fakeVector(text string) []float64 {
	v := make([]float64, fakeDims)
	v[fakeDims-1] = 0.01 // never all zero
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) }) {
		if d, ok := concepts[w]; ok {
			v[d] += 3
			continue
		}
		h := fnv.New32a()
		_, _ = h.Write([]byte(w))
		v[2+int(h.Sum32()%(fakeDims-3))]++
	}
	return v
}

// waitFor waits until cond holds, or fails the test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
