package conversations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/db"
)

const password = "a-long-enough-password"

// fakeRuntime is the agent runtime in memory. Its watches are fed by the test.
type fakeRuntime struct {
	mu      sync.Mutex
	next    int64
	models  map[int64]agentrt.ModelRef
	created []agentrt.ModelRef
	inputs  map[int64][]agentrt.Input
	watches []*fakeWatch
	aborts  map[int64]int
	err     error // returned by every call, when set
	// onAbort, when set, runs inside Abort with its context, before it returns.
	onAbort func(ctx context.Context)
	// replies are what the agent answered in each conversation, after the inputs. A tool's
	// result and the system's own text are in the transcript too, which Transcript leaves out.
	replies map[int64][]agentrt.Message
	// settle, when set, holds Wait until it is closed or its context ends.
	settle chan struct{}
	// waited counts the submissions Wait was called for.
	waited atomic.Int64
}

// setupToken is the first-run setup token the tests' server is given.
var setupToken = auth.NewSetupToken()

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{next: 100, models: map[int64]agentrt.ModelRef{}, inputs: map[int64][]agentrt.Input{}, aborts: map[int64]int{}}
}

var defaultModel = agentrt.ModelRef{Provider: "script", ModelID: "script-1"}

func (f *fakeRuntime) CreateConversationOn(_ context.Context, model agentrt.ModelRef) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.created = append(f.created, model)
	if model == (agentrt.ModelRef{}) {
		model = defaultModel
	}
	f.next++
	f.models[f.next] = model
	return f.next, nil
}

func (f *fakeRuntime) Submit(_ context.Context, conv int64, in agentrt.Input) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.inputs[conv] = append(f.inputs[conv], in)
	return int64(1000 + len(f.inputs[conv])), nil
}

func (f *fakeRuntime) Messages(_ context.Context, conv int64) ([]agentrt.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []agentrt.Message
	for _, in := range f.inputs[conv] {
		out = append(out, agentrt.Message{Role: "user", Text: in.Content})
	}
	return append(out, f.replies[conv]...), nil
}

func (f *fakeRuntime) Wait(ctx context.Context, _ int64) (agentrt.Settled, error) {
	f.waited.Add(1)
	f.mu.Lock()
	hold, err := f.settle, f.err
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return agentrt.Settled{}, ctx.Err()
		}
	}
	return agentrt.Settled{Status: "done"}, err
}

func (f *fakeRuntime) Model(_ context.Context, conv int64) (agentrt.ModelRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.models[conv], f.err
}

func (f *fakeRuntime) SetModel(_ context.Context, conv int64, model agentrt.ModelRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.models[conv] = model
	return nil
}

func (f *fakeRuntime) Abort(ctx context.Context, conv int64) error {
	if f.onAbort != nil {
		f.onAbort(ctx)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.aborts[conv]++
	return nil
}

// Entries answers each input as an entry, its ID its position from 1, so a test can page them.
func (f *fakeRuntime) Entries(_ context.Context, conv, before int64, limit int) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	type entry struct {
		ID      int64  `json:"id"`
		Content string `json:"content"`
	}
	var all []entry
	for i, in := range f.inputs[conv] {
		if id := int64(i + 1); before == 0 || id < before {
			all = append(all, entry{ID: id, Content: in.Content})
		}
	}
	page := struct {
		Entries []entry `json:"entries"`
		Before  int64   `json:"before,omitempty"`
	}{Entries: []entry{}}
	if len(all) > limit {
		page.Before = all[len(all)-limit-1].ID
		all = all[len(all)-limit:]
	}
	page.Entries = append(page.Entries, all...)
	return json.Marshal(page)
}

func (f *fakeRuntime) Watch(_ context.Context, conv int64) (Watch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	w := &fakeWatch{conv: conv, lines: make(chan string), closed: make(chan struct{})}
	f.watches = append(f.watches, w)
	return w, nil
}

// watch returns the n-th watch opened, waiting for it.
func (f *fakeRuntime) watch(t *testing.T, n int) *fakeWatch {
	t.Helper()
	var w *fakeWatch
	waitFor(t, "a watch to open", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.watches) > n {
			w = f.watches[n]
		}
		return w != nil
	})
	return w
}

// fakeWatch passes on the lines the test sends; closing lines ends it with io.EOF.
type fakeWatch struct {
	conv   int64
	lines  chan string
	mu     sync.Mutex
	nexts  int
	closed chan struct{}
	once   sync.Once
}

func (w *fakeWatch) Next() (agentrt.Event, error) {
	w.mu.Lock()
	w.nexts++
	w.mu.Unlock()
	select {
	case line, ok := <-w.lines:
		if !ok {
			return agentrt.Event{}, io.EOF
		}
		var ev agentrt.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return agentrt.Event{}, err
		}
		ev.Raw = json.RawMessage(line)
		return ev, nil
	case <-w.closed:
		return agentrt.Event{}, errors.New("watch closed")
	}
}

func (w *fakeWatch) Close() error {
	w.once.Do(func() { close(w.closed) })
	return nil
}

func (w *fakeWatch) isClosed() bool {
	select {
	case <-w.closed:
		return true
	default:
		return false
	}
}

// send passes line on, failing the test if the handler doesn't read it in time.
func (w *fakeWatch) send(t *testing.T, line string) {
	t.Helper()
	select {
	case w.lines <- line:
	case <-time.After(5 * time.Second):
		t.Fatalf("the handler didn't read %s", line)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type env struct {
	t   *testing.T
	db  *sql.DB
	rt  *fakeRuntime
	svc *Service
	h   http.Handler
}

// newEnv serves the API with a fake agent runtime; with off, without one.
func newEnv(t *testing.T, off bool) *env {
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
	e := &env{t: t, db: sqlDB, rt: newFakeRuntime()}
	var agent Agent
	if !off {
		agent = func(context.Context) (Runtime, error) { return e.rt, nil }
	}
	e.svc = New(sqlDB, agent)
	t.Cleanup(e.svc.EndStreams)
	mux := http.NewServeMux()
	a := auth.New(sqlDB, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
	a.Routes(mux)
	e.svc.Routes(mux, a)
	e.h = auth.CSRF(mux)
	return e
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
	return rec
}

func (c *client) expect(status int, method, path string, body any) *httptest.ResponseRecorder {
	c.e.t.Helper()
	rec := c.do(method, path, body)
	if rec.Code != status {
		c.e.t.Fatalf("%s %s = %d %s, want %d", method, path, rec.Code, rec.Body.String(), status)
	}
	return rec
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
	m := decode[me](e.t, c.expect(http.StatusCreated, http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": password, "workspace_name": "Home", "setup_token": setupToken,
	}))
	c.id = m.User.ID
	return c, m.Workspaces[0].ID
}

// invite adds a new person to the workspace as a member, and returns them logged in.
func (e *env) invite(admin *client, ws int64, email string) *client {
	e.t.Helper()
	token := decode[struct {
		Token string `json:"token"`
	}](e.t, admin.expect(http.StatusCreated, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", ws), map[string]string{"email": email}))
	c := &client{e: e}
	c.id = decode[me](e.t, c.expect(http.StatusCreated, http.MethodPost, "/api/invites/accept",
		map[string]string{"token": token.Token, "name": email, "password": password})).User.ID
	return c
}

// workspace creates another workspace with the given users as members; the server has no
// route for that yet.
func (e *env) workspace(members ...*client) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(`INSERT INTO workspaces (name) VALUES ('Other') RETURNING id`).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	for _, m := range members {
		if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, id, m.id); err != nil {
			e.t.Fatal(err)
		}
	}
	return id
}

// agentIDForTest reads the agent runtime's ID of c, which the API never shows.
func (c Conversation) agentIDForTest(t *testing.T, e *env) int64 {
	t.Helper()
	var id int64
	if err := e.db.QueryRow(`SELECT agent_id FROM conversations WHERE id = ?`, c.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func path(ws int64, rest string) string {
	return fmt.Sprintf("/api/workspaces/%d/conversations%s", ws, rest)
}

func (c *client) create(ws int64, body map[string]any) Conversation {
	c.e.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	return decode[Conversation](c.e.t, c.expect(http.StatusCreated, http.MethodPost, path(ws, ""), body))
}

func TestCreateListRenameArchive(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()

	first := owner.create(ws, map[string]any{"title": "  Plans  "})
	if first.Title != "Plans" || first.WorkspaceID != ws || first.ArchivedAt != nil || first.CreatedAt == "" {
		t.Fatalf("created %+v", first)
	}
	second := owner.create(ws, nil)
	if second.Title != "" {
		t.Fatalf("created %+v, want no title", second)
	}
	// The API's IDs are the server's, not the agent runtime's.
	if raw := owner.expect(http.StatusOK, http.MethodGet, path(ws, fmt.Sprintf("/%d", first.ID)), nil).Body.String(); bytes.Contains([]byte(raw), []byte("agent")) {
		t.Fatalf("the reply shows the agent runtime's ID: %s", raw)
	}

	list := func(query string) []int64 {
		t.Helper()
		var ids []int64
		for _, c := range decode[struct{ Conversations []Conversation }](t, owner.expect(http.StatusOK, http.MethodGet, path(ws, query), nil)).Conversations {
			ids = append(ids, c.ID)
		}
		return ids
	}
	if got := list(""); fmt.Sprint(got) != fmt.Sprint([]int64{second.ID, first.ID}) {
		t.Fatalf("list = %v, want the newest first", got)
	}

	// A message moves a conversation to the top.
	time.Sleep(2 * time.Millisecond) // updated_at has millisecond resolution
	owner.expect(http.StatusAccepted, http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", first.ID)), map[string]any{"content": "hi"})
	if got := list(""); fmt.Sprint(got) != fmt.Sprint([]int64{first.ID, second.ID}) {
		t.Fatalf("list after a message = %v", got)
	}

	one := path(ws, fmt.Sprintf("/%d", second.ID))
	renamed := decode[Conversation](t, owner.expect(http.StatusOK, http.MethodPatch, one, map[string]any{"title": "Renamed"}))
	if renamed.Title != "Renamed" || renamed.ArchivedAt != nil {
		t.Fatalf("renamed %+v", renamed)
	}
	archived := decode[Conversation](t, owner.expect(http.StatusOK, http.MethodPatch, one, map[string]any{"archived": true}))
	if archived.ArchivedAt == nil || archived.Title != "Renamed" {
		t.Fatalf("archived %+v", archived)
	}
	again := decode[Conversation](t, owner.expect(http.StatusOK, http.MethodPatch, one, map[string]any{"archived": true}))
	if *again.ArchivedAt != *archived.ArchivedAt {
		t.Fatalf("archiving again moved archived_at from %s to %s", *archived.ArchivedAt, *again.ArchivedAt)
	}
	if got := list(""); fmt.Sprint(got) != fmt.Sprint([]int64{first.ID}) {
		t.Fatalf("list = %v, want the archived one left out", got)
	}
	if got := list("?archived=true"); fmt.Sprint(got) != fmt.Sprint([]int64{second.ID}) {
		t.Fatalf("archived list = %v", got)
	}

	// Archived: readable, not writable.
	owner.expect(http.StatusOK, http.MethodGet, one+"/messages", nil)
	owner.expect(http.StatusConflict, http.MethodPost, one+"/messages", map[string]any{"content": "hi"})
	owner.expect(http.StatusConflict, http.MethodPut, one+"/model", map[string]any{"provider": "script", "model_id": "script-1"})
	owner.expect(http.StatusOK, http.MethodPatch, one, map[string]any{"archived": false})
	owner.expect(http.StatusAccepted, http.MethodPost, one+"/messages", map[string]any{"content": "hi"})

	for _, body := range []map[string]any{{}, {"title": string(make([]byte, 201))}, {"title": "a\x00b"}, {"owner_id": 2}} {
		owner.expect(http.StatusBadRequest, http.MethodPatch, one, body)
	}
	owner.expect(http.StatusBadRequest, http.MethodGet, path(ws, "?archived=maybe"), nil)
	owner.expect(http.StatusNotFound, http.MethodGet, path(ws, "/x"), nil)
	owner.expect(http.StatusNotFound, http.MethodGet, path(ws, "/999"), nil)
}

func TestSend(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	msgs := path(ws, fmt.Sprintf("/%d/messages", c.ID))

	got := decode[map[string]int64](t, owner.expect(http.StatusAccepted, http.MethodPost, msgs, map[string]any{"content": "hello"}))
	if got["submission_id"] == 0 {
		t.Fatalf("send = %v", got)
	}
	owner.expect(http.StatusAccepted, http.MethodPost, msgs, map[string]any{"content": "now this", "when_busy": "steer", "request_id": "r-1"})
	owner.expect(http.StatusAccepted, http.MethodPost, msgs, map[string]any{"content": "later", "when_busy": "follow_up"})
	for _, body := range []map[string]any{
		{"content": "  "},
		{"content": "x", "when_busy": "followUp"},
		{"content": "x", "when_busy": "reject"},
		{"content": "x", "request_id": string(bytes.Repeat([]byte("a"), 129))},
		{"content": "x", "request_id": "tab\there"},
		{"content": "x", "extra": true},
	} {
		owner.expect(http.StatusBadRequest, http.MethodPost, msgs, body)
	}

	e.rt.mu.Lock()
	inputs := e.rt.inputs[c.agentIDForTest(t, e)]
	e.rt.mu.Unlock()
	want := []agentrt.Input{{Content: "hello"}, {Content: "now this", WhenBusy: "steer", RequestID: "r-1"}, {Content: "later", WhenBusy: "followUp"}}
	if fmt.Sprint(inputs) != fmt.Sprint(want) {
		t.Fatalf("the agent runtime got %+v, want %+v", inputs, want)
	}

	read := decode[struct{ Messages []agentrt.Message }](t, owner.expect(http.StatusOK, http.MethodGet, msgs, nil))
	if len(read.Messages) != 3 || read.Messages[0].Text != "hello" {
		t.Fatalf("messages = %+v", read.Messages)
	}
}

// Stop reaches the agent runtime for the conversation, archived or not, since archiving doesn't
// stop a running turn.
func TestStop(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	one := path(ws, fmt.Sprintf("/%d", c.ID))
	owner.expect(http.StatusNoContent, http.MethodPost, one+"/stop", map[string]any{})
	owner.expect(http.StatusOK, http.MethodPatch, one, map[string]any{"archived": true})
	owner.expect(http.StatusNoContent, http.MethodPost, one+"/stop", map[string]any{})
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	if got := e.rt.aborts[c.agentIDForTest(t, e)]; got != 2 {
		t.Fatalf("the agent runtime got %d aborts, want 2", got)
	}
}

// Entries pages back through the history, passing the agent runtime's page on unchanged.
func TestEntries(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	one := path(ws, fmt.Sprintf("/%d", c.ID))
	for _, m := range []string{"one", "two", "three"} {
		owner.expect(http.StatusAccepted, http.MethodPost, one+"/messages", map[string]any{"content": m})
	}
	type page struct {
		Entries []struct {
			ID      int64
			Content string
		}
		Before int64
	}
	got := decode[page](t, owner.expect(http.StatusOK, http.MethodGet, one+"/entries?before=3&limit=1", nil))
	if len(got.Entries) != 1 || got.Entries[0].Content != "two" || got.Before != 1 {
		t.Fatalf("page before 3 = %+v, want two, with one before it", got)
	}
	got = decode[page](t, owner.expect(http.StatusOK, http.MethodGet, one+"/entries?before=2", nil))
	if len(got.Entries) != 1 || got.Entries[0].Content != "one" || got.Before != 0 {
		t.Fatalf("page before 2 = %+v, want one, starting the transcript", got)
	}
	got = decode[page](t, owner.expect(http.StatusOK, http.MethodGet, one+"/entries", nil))
	if len(got.Entries) != 3 {
		t.Fatalf("newest page = %+v, want all three", got)
	}
	for _, q := range []string{"before=0", "before=-1", "before=x", "limit=0", "limit=101", "limit=x"} {
		owner.expect(http.StatusBadRequest, http.MethodGet, one+"/entries?"+q, nil)
	}
	owner.expect(http.StatusOK, http.MethodGet, one+"/entries?limit=100", nil)
}

func TestModelChoiceFollowsWhoMayUseTheProvider(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com")
	other := e.workspace(owner, member)

	ref := func(provider string) map[string]any {
		return map[string]any{"provider": provider, "model_id": "m"}
	}
	mine := fmt.Sprintf("u%d-home", owner.id)
	theirs := fmt.Sprintf("u%d-home", member.id)
	thisWS := fmt.Sprintf("ws%d-gateway", ws)
	otherWS := fmt.Sprintf("ws%d-gateway", other)

	// Usable: the operator's providers, the caller's own, and the workspace's.
	for _, p := range []string{"script", "gateway", mine, thisWS} {
		c := decode[Conversation](t, owner.expect(http.StatusCreated, http.MethodPost, path(ws, ""), map[string]any{"model": ref(p)}))
		got := decode[modelRef](t, owner.expect(http.StatusOK, http.MethodGet, path(ws, fmt.Sprintf("/%d/model", c.ID)), nil))
		if got.Provider != p || got.ModelID != "m" {
			t.Fatalf("model = %+v, want %s/m", got, p)
		}
	}
	// Refused, the same as a model that doesn't exist: someone else's personal provider,
	// another workspace's, and malformed choices.
	conv := owner.create(ws, nil)
	model := path(ws, fmt.Sprintf("/%d/model", conv.ID))
	for _, p := range []string{theirs, otherWS, "u0-x", "ws0-x"} {
		rec := owner.expect(http.StatusBadRequest, http.MethodPost, path(ws, ""), map[string]any{"model": ref(p)})
		if msg := decode[map[string]string](t, rec)["error"]; msg != "unknown model" {
			t.Fatalf("create on %s: error %q", p, msg)
		}
		owner.expect(http.StatusBadRequest, http.MethodPut, model, ref(p))
	}
	for _, body := range []map[string]any{{"provider": "script"}, {"model_id": "m"}, {"provider": "", "model_id": ""}} {
		owner.expect(http.StatusBadRequest, http.MethodPut, model, body)
		owner.expect(http.StatusBadRequest, http.MethodPost, path(ws, ""), map[string]any{"model": body})
	}
	// The other workspace's provider is usable there, and the member's in their own conversation.
	owner.expect(http.StatusCreated, http.MethodPost, path(other, ""), map[string]any{"model": ref(otherWS)})
	owner.expect(http.StatusBadRequest, http.MethodPost, path(other, ""), map[string]any{"model": ref(thisWS)})
	memberConv := member.create(ws, map[string]any{"model": ref(theirs)})
	member.expect(http.StatusBadRequest, http.MethodPut, path(ws, fmt.Sprintf("/%d/model", memberConv.ID)), ref(mine))
	member.expect(http.StatusOK, http.MethodPut, path(ws, fmt.Sprintf("/%d/model", memberConv.ID)), ref(thisWS))

	// Only the allowed choices reached the agent runtime.
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	var got []string
	for _, m := range e.rt.created {
		got = append(got, m.Provider)
	}
	if want := []string{"script", "gateway", mine, thisWS, "", otherWS, theirs}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the agent runtime created conversations on %q, want %q", got, want)
	}
	if m := e.rt.models[memberConv.agentIDForTest(t, e)]; m.Provider != thisWS {
		t.Fatalf("the member's conversation runs on %+v", m)
	}
}

// Every route on someone else's conversation answers 404, as for one that doesn't exist:
// another member of the workspace, a member of another workspace, the owner through another
// workspace's path, and the owner once they have left the workspace.
func TestConversationsArePrivateToTheirOwner(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com")
	stranger := &client{e: e}
	strangerWS := func() int64 {
		// A user who is in another workspace only.
		c := e.invite(owner, ws, "stranger@example.com")
		other := e.workspace(c)
		owner.expect(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, c.id), nil)
		*stranger = *c
		return other
	}()
	conv := owner.create(ws, map[string]any{"title": "secret"})
	memberConv := member.create(ws, nil)

	routes := func(ws, id int64) [][3]any {
		one := path(ws, fmt.Sprintf("/%d", id))
		return [][3]any{
			{http.MethodGet, one, nil},
			{http.MethodPatch, one, map[string]any{"title": "mine now"}},
			{http.MethodPatch, one, map[string]any{"archived": true}},
			{http.MethodPatch, one, map[string]any{"cli_permissions": "allow_all"}},
			{http.MethodGet, one + "/cli-permissions", nil},
			{http.MethodPost, one + "/cli-permissions/q1", map[string]any{"allow": true}},
			{http.MethodPost, one + "/messages", map[string]any{"content": "hi"}},
			{http.MethodGet, one + "/messages", nil},
			{http.MethodGet, one + "/model", nil},
			{http.MethodPut, one + "/model", map[string]any{"provider": "script", "model_id": "script-1"}},
			{http.MethodPost, one + "/stop", map[string]any{}},
			{http.MethodGet, one + "/entries", nil},
			{http.MethodGet, one + "/events", nil},
		}
	}
	denied := func(c *client, ws, id int64) {
		t.Helper()
		for _, r := range routes(ws, id) {
			rec := c.do(r[0].(string), r[1].(string), r[2])
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s = %d %s, want 404", r[0], r[1], rec.Code, rec.Body.String())
			}
		}
	}
	denied(member, ws, conv.ID)
	denied(owner, ws, memberConv.ID) // admins too
	denied(stranger, ws, conv.ID)
	denied(stranger, strangerWS, conv.ID)
	other := e.workspace(owner)
	denied(owner, other, conv.ID)
	listed := decode[struct{ Conversations []Conversation }](t, member.expect(http.StatusOK, http.MethodGet, path(ws, ""), nil))
	if len(listed.Conversations) != 1 || listed.Conversations[0].ID != memberConv.ID {
		t.Fatalf("the member lists %+v, want only their own", listed.Conversations)
	}
	stranger.expect(http.StatusNotFound, http.MethodGet, path(ws, ""), nil)
	stranger.expect(http.StatusNotFound, http.MethodPost, path(ws, ""), map[string]any{})
	(&client{e: e}).expect(http.StatusUnauthorized, http.MethodGet, path(ws, ""), nil)

	// Nothing changed, and the agent runtime got nothing.
	got := decode[Conversation](t, owner.expect(http.StatusOK, http.MethodGet, path(ws, fmt.Sprintf("/%d", conv.ID)), nil))
	if got.Title != "secret" || got.ArchivedAt != nil {
		t.Fatalf("the conversation changed: %+v", got)
	}
	e.rt.mu.Lock()
	if n := len(e.rt.inputs) + len(e.rt.watches) + len(e.rt.aborts); n != 0 {
		t.Fatalf("the agent runtime got %d calls for denied requests", n)
	}
	e.rt.mu.Unlock()

	// A member who leaves loses their conversations, and gets them back on rejoining.
	owner.expect(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, member.id), nil)
	denied(member, ws, memberConv.ID)
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, ws, member.id); err != nil {
		t.Fatal(err)
	}
	member.expect(http.StatusOK, http.MethodGet, path(ws, fmt.Sprintf("/%d", memberConv.ID)), nil)
}

func TestAgentRuntimeOff(t *testing.T) {
	e := newEnv(t, true)
	owner, ws := e.setup()
	rec := owner.expect(http.StatusServiceUnavailable, http.MethodPost, path(ws, ""), map[string]any{})
	if msg := decode[map[string]string](t, rec)["error"]; msg != "the agent runtime is off: start the server with -agent" {
		t.Fatalf("error = %q", msg)
	}
	// A conversation from when it was on: what needs only the database still works.
	if _, err := e.db.Exec(`INSERT INTO conversations (workspace_id, owner_id, agent_id) VALUES (?, ?, 7)`, ws, owner.id); err != nil {
		t.Fatal(err)
	}
	owner.expect(http.StatusOK, http.MethodGet, path(ws, ""), nil)
	owner.expect(http.StatusOK, http.MethodGet, path(ws, "/1"), nil)
	owner.expect(http.StatusOK, http.MethodPatch, path(ws, "/1"), map[string]any{"title": "Old"})
	for _, r := range [][3]any{
		{http.MethodPost, "/1/messages", map[string]any{"content": "hi"}},
		{http.MethodGet, "/1/messages", nil},
		{http.MethodGet, "/1/model", nil},
		{http.MethodPut, "/1/model", map[string]any{"provider": "script", "model_id": "script-1"}},
		{http.MethodPost, "/1/stop", map[string]any{}},
		{http.MethodGet, "/1/entries", nil},
		{http.MethodGet, "/1/events", nil},
	} {
		owner.expect(http.StatusServiceUnavailable, r[0].(string), path(ws, r[1].(string)), r[2])
	}
	// Access comes first: someone else's conversation is still a 404.
	member := e.invite(owner, ws, "member@example.com")
	member.expect(http.StatusNotFound, http.MethodGet, path(ws, "/1/messages"), nil)
}

func TestAgentRuntimeNotReady(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	conv := owner.create(ws, nil)
	e.svc.readyWait = 20 * time.Millisecond
	e.svc.agent = func(ctx context.Context) (Runtime, error) {
		<-ctx.Done() // restarting, for longer than a request waits
		return nil, ctx.Err()
	}
	rec := owner.expect(http.StatusServiceUnavailable, http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", conv.ID)), map[string]any{"content": "hi"})
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	if msg := decode[map[string]string](t, rec)["error"]; msg != "the agent runtime isn't ready: try again shortly" {
		t.Fatalf("error = %q", msg)
	}
}

func TestAgentRuntimeErrors(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	conv := owner.create(ws, nil)
	msgs := path(ws, fmt.Sprintf("/%d/messages", conv.ID))
	for _, tc := range []struct {
		err    error
		status int
		msg    string
		code   apierr.Code
	}{
		{&agentrt.StatusError{Code: 400, Message: "unknown model", Reason: "unknown_model"}, 400, "unknown model", apierr.CodeModelUnknown},
		{&agentrt.StatusError{Code: 409, Message: "conversation has no model", Reason: "no_model"}, 409, "conversation has no model", apierr.CodeConversationNoModel},
		{&agentrt.StatusError{Code: 400, Message: "invalid id"}, 400, "invalid id", apierr.CodeBadRequest},
		{&agentrt.StatusError{Code: 404, Message: "unknown conversation"}, 410, "the agent runtime no longer has this conversation's content", apierr.CodeConversationLost},
		{&agentrt.StatusError{Code: 500, Message: "boom"}, 500, "internal error", apierr.CodeInternal},
		{errors.New("dial unix /secret/path/agent.sock: connect: no such file"), 503, "the agent runtime isn't ready: try again shortly", apierr.CodeAgentNotReady},
	} {
		e.rt.mu.Lock()
		e.rt.err = tc.err
		e.rt.mu.Unlock()
		rec := owner.do(http.MethodGet, msgs, nil)
		body := decode[map[string]string](t, rec)
		if rec.Code != tc.status || body["error"] != tc.msg || body["code"] != string(tc.code) {
			t.Errorf("%v: %d %v, want %d %q %q", tc.err, rec.Code, body, tc.status, tc.msg, tc.code)
		}
	}
}

// The update re-checks access in its own statement: someone removed from the workspace
// after the lookup can't rename or archive their old conversation.
func TestUpdateRechecksAccess(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com")
	conv := member.create(ws, nil)
	owner.expect(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, member.id), nil)
	title := "renamed after leaving"
	if _, err := e.svc.update(context.Background(), ws, member.id, conv.ID, &title, nil, nil); !errors.Is(err, errNotFound) {
		t.Fatalf("update = %v, want errNotFound", err)
	}
	var got string
	if err := e.db.QueryRow(`SELECT title FROM conversations WHERE id = ?`, conv.ID).Scan(&got); err != nil || got == title {
		t.Fatalf("title = %q, %v; want it unchanged", got, err)
	}
}

// captureLogs sends the default logger to a buffer for the rest of the test.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A conversation the agent runtime has lost (its data wiped, or restored apart from the
// server's) is 410 on every route that reaches it, and the mismatch is logged with both IDs.
func TestLostConversationIsGone(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	conv := owner.create(ws, nil)
	logs := captureLogs(t)
	e.rt.mu.Lock()
	e.rt.err = &agentrt.StatusError{Code: 404, Message: "unknown conversation"}
	e.rt.mu.Unlock()
	for _, r := range [][3]any{
		{http.MethodPost, "/messages", map[string]any{"content": "hi"}},
		{http.MethodGet, "/messages", nil},
		{http.MethodGet, "/model", nil},
		{http.MethodPut, "/model", map[string]any{"provider": "script", "model_id": "script-1"}},
		{http.MethodPost, "/stop", map[string]any{}},
		{http.MethodGet, "/entries", nil},
		{http.MethodGet, "/events", nil},
	} {
		rec := owner.expect(http.StatusGone, r[0].(string), path(ws, fmt.Sprintf("/%d%s", conv.ID, r[1])), r[2])
		if msg := decode[map[string]string](t, rec)["error"]; msg != "the agent runtime no longer has this conversation's content" {
			t.Errorf("%s %s: error = %q", r[0], r[1], msg)
		}
	}
	want := fmt.Sprintf("conversation=%d agent_conversation=%d workspace=%d", conv.ID, conv.agentIDForTest(t, e), ws)
	if got := logs.String(); strings.Count(got, want) != 7 {
		t.Fatalf("logs = %s; want the mismatch logged for each request with %q", got, want)
	}
	// What needs only the database still works.
	owner.expect(http.StatusOK, http.MethodGet, path(ws, fmt.Sprintf("/%d", conv.ID)), nil)
}

// Send and model switch check the conversation again once the agent runtime is ready: an
// archive or a removal from the workspace while the request waited for it is honoured.
func TestSendAndModelSwitchRecheckBeforeTheCall(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com")
	ready := func(change string, args ...any) {
		e.svc.agent = func(context.Context) (Runtime, error) {
			if _, err := e.db.Exec(change, args...); err != nil {
				t.Error(err)
			}
			return e.rt, nil
		}
	}
	model := map[string]any{"provider": "script", "model_id": "script-1"}

	archived := owner.create(ws, nil)
	ready(`UPDATE conversations SET archived_at = 'now' WHERE id = ?`, archived.ID)
	owner.expect(http.StatusConflict, http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", archived.ID)), map[string]any{"content": "hi"})
	owner.expect(http.StatusConflict, http.MethodPut, path(ws, fmt.Sprintf("/%d/model", archived.ID)), model)

	removed := member.create(ws, nil)
	ready(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, ws, member.id)
	member.expect(http.StatusNotFound, http.MethodPost, path(ws, fmt.Sprintf("/%d/messages", removed.ID)), map[string]any{"content": "hi"})
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, ws, member.id); err != nil {
		t.Fatal(err)
	}
	member.expect(http.StatusNotFound, http.MethodPut, path(ws, fmt.Sprintf("/%d/model", removed.ID)), model)

	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	if len(e.rt.inputs) != 0 {
		t.Fatalf("the agent runtime got %v, want nothing", e.rt.inputs)
	}
	for _, c := range []Conversation{archived, removed} {
		if got := e.rt.models[c.agentIDForTest(t, e)]; got != defaultModel {
			t.Fatalf("model of %d = %+v, want it unchanged", c.ID, got)
		}
	}
}

// A stop carries on when the client goes away mid-way: its outcome must not be left unknown.
func TestStopOutlivesTheRequest(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	ctx, cancel := context.WithCancel(context.Background())
	var abortErr error
	e.rt.onAbort = func(actx context.Context) {
		cancel() // the client leaves while the agent runtime is stopping the work
		abortErr = actx.Err()
	}
	r := httptest.NewRequest(http.MethodPost, path(ws, fmt.Sprintf("/%d/stop", c.ID)), bytes.NewReader([]byte("{}"))).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.AddCookie(owner.cookie)
	e.h.ServeHTTP(httptest.NewRecorder(), r)
	if abortErr != nil {
		t.Fatalf("the stop's context ended with the request: %v", abortErr)
	}
}
