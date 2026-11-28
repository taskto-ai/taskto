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
	"taskto.ai/taskto/internal/providers"
)

const password = "a-long-enough-password"

type fakeRuntime struct {
	mu            sync.Mutex
	next          int64
	models        map[int64]agentrt.ModelRef
	revs          map[int64]int64
	created       []agentrt.ModelRef
	inputs        map[int64][]agentrt.Input
	notes         map[int64][]agentrt.NoteInput
	calls         []string
	watches       []*fakeWatch
	aborts        map[int64]int
	resumes       map[int64]int
	resumed       []continueCall
	sets          int
	err           error
	onAbort       func(ctx context.Context)
	replies       map[int64][]agentrt.Message
	rawEntries    map[int64][]json.RawMessage
	settle        chan struct{}
	waited        atomic.Int64
	waitErr       error
	submitErr     error
	onSubmit      func()
	stallSend     bool
	stallPutBack  bool
	onEntries     func()
	onContinue    func()
	onSetModel    func()
	stallContinue bool
	continueErr   error

	lateContinue  bool
	continueCalls int
	landing       func() error

	enforceAdmitBy bool
	deadlines      []time.Time
}

var errModelChanged = &agentrt.StatusError{Code: http.StatusConflict, Message: "the conversation is on another model", Reason: "model_changed"}

var errTooLate = &agentrt.StatusError{Code: http.StatusServiceUnavailable, Message: "the time to admit this request has passed", Reason: "too_late"}

func (f *fakeRuntime) expects(conv int64, expected *int64) error {
	if expected != nil && f.revs[conv] != *expected {
		return errModelChanged
	}
	return nil
}

func (f *fakeRuntime) write(conv int64, model agentrt.ModelRef) int64 {
	f.models[conv] = model
	f.revs[conv]++
	return f.revs[conv]
}

func (f *fakeRuntime) admitted(by time.Time) error {
	if f.enforceAdmitBy && !by.IsZero() && time.Now().After(by) {
		return errTooLate
	}
	return nil
}

var setupToken = auth.NewSetupToken()

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{next: 100, models: map[int64]agentrt.ModelRef{}, revs: map[int64]int64{}, inputs: map[int64][]agentrt.Input{}, notes: map[int64][]agentrt.NoteInput{}, aborts: map[int64]int{}, resumes: map[int64]int{}}
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

func (f *fakeRuntime) Submit(ctx context.Context, conv int64, in agentrt.Input) (int64, error) {
	f.mu.Lock()
	hook := f.onSubmit
	stall := f.stallSend
	by, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, by)
	f.mu.Unlock()
	if stall {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	if f.submitErr != nil {
		return 0, f.submitErr
	}
	if err := f.admitted(by); err != nil {
		return 0, err
	}
	if err := f.expects(conv, in.ExpectRevision); err != nil {
		return 0, err
	}
	f.inputs[conv] = append(f.inputs[conv], in)
	f.calls = append(f.calls, "submit")
	return int64(1000 + len(f.inputs[conv])), nil
}

type continueCall struct {
	model agentrt.ModelRef
	after int64
}

func (f *fakeRuntime) Continue(ctx context.Context, conv, revision int64, model agentrt.ModelRef, after int64) (int64, agentrt.ModelRef, error) {
	if f.onContinue != nil {
		f.onContinue()
	}
	if f.stallContinue {
		<-ctx.Done()
		return 0, agentrt.ModelRef{}, ctx.Err()
	}
	f.mu.Lock()
	f.continueCalls++
	by, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, by)
	if f.lateContinue && f.continueCalls == 1 {
		f.landing = func() error {
			f.mu.Lock()
			defer f.mu.Unlock()
			_, _, err := f.admitContinue(conv, revision, by, model, after)
			return err
		}
		f.mu.Unlock()
		<-ctx.Done()
		return 0, agentrt.ModelRef{}, ctx.Err()
	}
	defer f.mu.Unlock()
	if f.continueErr != nil {
		return 0, agentrt.ModelRef{}, f.continueErr
	}
	if f.err != nil {
		return 0, agentrt.ModelRef{}, f.err
	}
	return f.admitContinue(conv, revision, by, model, after)
}

func (f *fakeRuntime) admitContinue(conv, revision int64, by time.Time, model agentrt.ModelRef, after int64) (int64, agentrt.ModelRef, error) {
	if err := f.admitted(by); err != nil {
		return 0, agentrt.ModelRef{}, err
	}
	if err := f.expects(conv, &revision); err != nil {
		return 0, agentrt.ModelRef{}, err
	}
	f.write(conv, model)
	f.resumed = append(f.resumed, continueCall{model, after})
	f.resumes[conv]++
	return int64(3000 + f.resumes[conv]), model, nil
}

func (f *fakeRuntime) landLate() error {
	f.mu.Lock()
	land := f.landing
	f.mu.Unlock()
	return land()
}

func (f *fakeRuntime) Note(_ context.Context, conv int64, n agentrt.NoteInput) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.notes[conv] = append(f.notes[conv], n)
	return int64(2000 + len(f.notes[conv])), nil
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
	if f.waitErr != nil {
		err = f.waitErr
	}
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

func (f *fakeRuntime) Model(_ context.Context, conv int64) (agentrt.ModelState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return agentrt.ModelState{ModelRef: f.models[conv], Revision: f.revs[conv]}, f.err
}

func (f *fakeRuntime) SetModel(ctx context.Context, conv, revision int64, model agentrt.ModelRef) (int64, error) {
	if f.onSetModel != nil {
		f.onSetModel()
	}
	f.mu.Lock()
	if f.stallPutBack && len(f.calls) > 0 && strings.HasPrefix(f.calls[len(f.calls)-1], "set-model ") {
		f.mu.Unlock()
		<-ctx.Done()
		return 0, ctx.Err()
	}
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	if err := f.expects(conv, &revision); err != nil {
		return 0, err
	}
	next := f.write(conv, model)
	f.calls = append(f.calls, "set-model "+model.Provider+"/"+model.ModelID)
	f.sets++
	return next, nil
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

func (f *fakeRuntime) Entries(_ context.Context, conv, before int64, limit int) (json.RawMessage, error) {
	if f.onEntries != nil {
		f.onEntries()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if entries, ok := f.rawEntries[conv]; ok {
		var all []json.RawMessage
		for _, raw := range entries {
			var entry struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal(raw, &entry) == nil && (before == 0 || entry.ID < before) {
				all = append(all, raw)
			}
		}
		page := struct {
			Entries []json.RawMessage `json:"entries"`
			Before  int64             `json:"before,omitempty"`
		}{Entries: []json.RawMessage{}}
		if len(all) > limit {
			var entry struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(all[len(all)-limit-1], &entry)
			page.Before = entry.ID
			all = all[len(all)-limit:]
		}
		page.Entries = append(page.Entries, all...)
		return json.Marshal(page)
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

	runnable []providers.Choice
}

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
	e.svc.admitLimit, e.svc.admitMargin = 300*time.Millisecond, 20*time.Millisecond
	e.runnable = []providers.Choice{
		{Provider: "script", Name: "Scripted", Scope: providers.ScopeInstance, Models: []providers.ModelChoice{{ID: "script-1", Name: "Scripted"}}},
		{Provider: "backup", Name: "Backup", Scope: providers.ScopeInstance, Models: []providers.ModelChoice{{ID: "b-1", Name: "B one"}}},
	}
	e.svc.SetContinueModels(func(context.Context, Principal) ([]providers.Choice, error) { return e.runnable, nil })
	t.Cleanup(e.svc.EndStreams)
	mux := http.NewServeMux()
	a := auth.New(sqlDB, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
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

func (e *env) setup() (*client, int64) {
	e.t.Helper()
	c := &client{e: e}
	m := decode[me](e.t, c.expect(http.StatusCreated, http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": password, "workspace_name": "Home", "setup_token": setupToken,
	}))
	c.id = m.User.ID
	return c, m.Workspaces[0].ID
}

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

	time.Sleep(2 * time.Millisecond)
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
	if len(inputs) != len(want) {
		t.Fatalf("the agent runtime got %+v, want %d messages", inputs, len(want))
	}
	for i := range want {
		if inputs[i].Content != want[i].Content || inputs[i].WhenBusy != want[i].WhenBusy || inputs[i].RequestID != want[i].RequestID {
			t.Fatalf("the agent runtime got %+v, want %+v", inputs[i], want[i])
		}
		var data struct {
			V      int   `json:"v"`
			Author int64 `json:"author"`
		}
		if inputs[i].Entry == nil || inputs[i].Entry.Kind != "taskto.input" || json.Unmarshal(inputs[i].Entry.Data, &data) != nil || data.V != 1 || data.Author != owner.id {
			t.Fatalf("the agent runtime got input entry %+v, want taskto.input by %d", inputs[i].Entry, owner.id)
		}
	}

	read := decode[struct{ Messages []agentrt.Message }](t, owner.expect(http.StatusOK, http.MethodGet, msgs, nil))
	if len(read.Messages) != 3 || read.Messages[0].Text != "hello" {
		t.Fatalf("messages = %+v", read.Messages)
	}
}

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

	for _, p := range []string{"script", "gateway", mine, thisWS} {
		c := decode[Conversation](t, owner.expect(http.StatusCreated, http.MethodPost, path(ws, ""), map[string]any{"model": ref(p)}))
		got := decode[modelRef](t, owner.expect(http.StatusOK, http.MethodGet, path(ws, fmt.Sprintf("/%d/model", c.ID)), nil))
		if got.Provider != p || got.ModelID != "m" {
			t.Fatalf("model = %+v, want %s/m", got, p)
		}
	}
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
	owner.expect(http.StatusCreated, http.MethodPost, path(other, ""), map[string]any{"model": ref(otherWS)})
	owner.expect(http.StatusBadRequest, http.MethodPost, path(other, ""), map[string]any{"model": ref(thisWS)})
	memberConv := member.create(ws, map[string]any{"model": ref(theirs)})
	member.expect(http.StatusBadRequest, http.MethodPut, path(ws, fmt.Sprintf("/%d/model", memberConv.ID)), ref(mine))
	member.expect(http.StatusOK, http.MethodPut, path(ws, fmt.Sprintf("/%d/model", memberConv.ID)), ref(thisWS))

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

func TestConversationsArePrivateToTheirOwner(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com")
	stranger := &client{e: e}
	strangerWS := func() int64 {
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
			{http.MethodPost, one + "/notes", map[string]any{"content": "a note"}},
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
	denied(owner, ws, memberConv.ID)
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

	got := decode[Conversation](t, owner.expect(http.StatusOK, http.MethodGet, path(ws, fmt.Sprintf("/%d", conv.ID)), nil))
	if got.Title != "secret" || got.ArchivedAt != nil {
		t.Fatalf("the conversation changed: %+v", got)
	}
	e.rt.mu.Lock()
	if n := len(e.rt.inputs) + len(e.rt.notes) + len(e.rt.watches) + len(e.rt.aborts); n != 0 {
		t.Fatalf("the agent runtime got %d calls for denied requests", n)
	}
	e.rt.mu.Unlock()

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
	member := e.invite(owner, ws, "member@example.com")
	member.expect(http.StatusNotFound, http.MethodGet, path(ws, "/1/messages"), nil)
}

func TestAgentRuntimeNotReady(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	conv := owner.create(ws, nil)
	e.svc.readyWait = 20 * time.Millisecond
	e.svc.agent = func(ctx context.Context) (Runtime, error) {
		<-ctx.Done()
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
	owner.expect(http.StatusOK, http.MethodGet, path(ws, fmt.Sprintf("/%d", conv.ID)), nil)
}

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

func TestStopOutlivesTheRequest(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	ctx, cancel := context.WithCancel(context.Background())
	var abortErr error
	e.rt.onAbort = func(actx context.Context) {
		cancel()
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
