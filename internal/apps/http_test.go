package apps

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/integrations"
	"taskto.ai/taskto/internal/serverkey"
)

// Test passwords: obviously fake, and long enough for the account rules.
const fakePassword = "fake-test-password"

var setupToken = auth.NewSetupToken()

// env serves the account, space and app routes behind the CSRF guard, as the server does.
type env struct {
	t  *testing.T
	db *sql.DB
	s  *Service
	h  http.Handler
	// dataDir is where the spaces' files are.
	dataDir string
	// dbHooks run what a test sets at chosen moments of the database's use.
	dbHooks *db.Hooks
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	dbHooks := &db.Hooks{}
	sqlDB, err := db.OpenHooked(ctx, migratedDatabase(t, t.TempDir()), dbHooks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	accounts := auth.New(sqlDB, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
	spaces := access.New(sqlDB, accounts)
	dataDir := t.TempDir()
	s := New(sqlDB, spaces).WithFiles(dataDir)
	mux := http.NewServeMux()
	accounts.Routes(mux)
	spaces.Routes(mux)
	s.Routes(mux)
	s.ApprovalRoutes(mux, accounts)
	s.HomeRoutes(mux, accounts)
	s.TemplateRoutes(mux, accounts)
	key, err := serverkey.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	integrations.New(integrations.NewStore(sqlDB, key), integrations.Options{}).Routes(mux, accounts, spaces)
	return &env{t: t, db: sqlDB, s: s, h: auth.CSRF(mux, RawBodyRoutes...), dataDir: dataDir, dbHooks: dbHooks}
}

// user is a logged-in browser.
type user struct {
	e      *env
	id     int64
	cookie *http.Cookie
}

func (u *user) do(method, path string, body any) *httptest.ResponseRecorder {
	u.e.t.Helper()
	return u.doWith(nil, method, path, body)
}

// doWith is do with more request headers.
func (u *user) doWith(headers map[string]string, method, path string, body any) *httptest.ResponseRecorder {
	u.e.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			u.e.t.Fatal(err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
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

func (e *env) setup() (*user, int64) {
	e.t.Helper()
	owner := &user{e: e}
	rec := owner.do(http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": fakePassword, "workspace_name": "Home", "setup_token": setupToken,
	})
	expect(e.t, rec, http.StatusCreated)
	me := decode[meReply](e.t, rec)
	owner.id = me.User.ID
	return owner, me.Workspaces[0].WorkspaceID
}

func (e *env) join(admin *user, workspace int64, name string) *user {
	e.t.Helper()
	rec := admin.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", workspace),
		map[string]string{"email": strings.ToLower(name) + "@example.com", "role": "member"})
	expect(e.t, rec, http.StatusCreated)
	token := decode[struct{ Token string }](e.t, rec).Token
	u := &user{e: e}
	rec = u.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token, "name": name, "password": fakePassword})
	expect(e.t, rec, http.StatusCreated)
	u.id = decode[meReply](e.t, rec).User.ID
	return u
}

func (e *env) createShared(admin *user, workspace int64, name string) access.Space {
	e.t.Helper()
	rec := admin.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces", workspace), map[string]string{"name": name})
	expect(e.t, rec, http.StatusCreated)
	return decode[access.Space](e.t, rec)
}

func (e *env) addMember(admin *user, s access.Space, u *user, role access.Role) {
	e.t.Helper()
	expect(e.t, admin.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members", s.WorkspaceID, s.ID),
		map[string]any{"user_id": u.id, "role": role}), http.StatusNoContent)
}

func (e *env) private(u *user, workspace int64) access.Space {
	e.t.Helper()
	rec := u.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces", workspace), nil)
	expect(e.t, rec, http.StatusOK)
	for _, s := range decode[struct{ Spaces []access.Space }](e.t, rec).Spaces {
		if s.Kind == access.Private {
			return s
		}
	}
	e.t.Fatalf("user %d has no private space", u.id)
	return access.Space{}
}

func appsPath(s access.Space, rest string) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d/apps%s", s.WorkspaceID, s.ID, rest)
}

func appPath(s access.Space, app int64, rest string) string {
	return appsPath(s, fmt.Sprintf("/%d%s", app, rest))
}

// world is a workspace with people in every relation to its spaces:
//
//   - owner: admin, editor of shared;
//   - alice: editor of shared and of old (an archived space);
//   - bob: viewer of shared;
//   - mallory: a member of the workspace, of no shared space;
//   - stranger: admin of another workspace, other, with a shared space of its own.
//
// Each private space of alice and bob has an app with a row, and so do shared and old.
type world struct {
	e                                   *env
	ws, other                           int64
	owner, alice, bob, mallory          *user
	stranger                            *user
	shared, old, otherShared            access.Space
	alicePrivate, bobPrivate            access.Space
	aliceApp, bobApp, sharedApp, oldApp App
}

func newWorld(t *testing.T) *world {
	t.Helper()
	e := newEnv(t)
	w := &world{e: e}
	w.owner, w.ws = e.setup()
	w.alice = e.join(w.owner, w.ws, "Alice")
	w.bob = e.join(w.owner, w.ws, "Bob")
	w.mallory = e.join(w.owner, w.ws, "Mallory")
	w.stranger = e.join(w.owner, w.ws, "Stranger")
	w.shared = e.createShared(w.owner, w.ws, "Team")
	e.addMember(w.owner, w.shared, w.alice, access.RoleEditor)
	e.addMember(w.owner, w.shared, w.bob, access.RoleViewer)
	w.old = e.createShared(w.owner, w.ws, "Old")
	e.addMember(w.owner, w.old, w.alice, access.RoleEditor)
	w.alicePrivate = e.private(w.alice, w.ws)
	w.bobPrivate = e.private(w.bob, w.ws)

	w.aliceApp = w.create(w.alice, w.alicePrivate, "Alice's expenses")
	w.add(w.alice, w.alicePrivate, w.aliceApp, "alice-secret", "1")
	w.bobApp = w.create(w.bob, w.bobPrivate, "Bob's expenses")
	w.add(w.bob, w.bobPrivate, w.bobApp, "bob-secret", "2")
	w.sharedApp = w.create(w.alice, w.shared, "Team expenses")
	w.add(w.alice, w.shared, w.sharedApp, "team-lunch", "3")
	w.oldApp = w.create(w.alice, w.old, "Old expenses")
	w.add(w.alice, w.old, w.oldApp, "old-row", "4")
	expect(t, w.owner.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/spaces/%d", w.ws, w.old.ID), map[string]bool{"archived": true}), http.StatusOK)

	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", w.ws, w.stranger.id), nil), http.StatusNoContent)
	if err := e.db.QueryRow(`INSERT INTO workspaces (name) VALUES ('Elsewhere') RETURNING id`).Scan(&w.other); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, w.other, w.stranger.id); err != nil {
		t.Fatal(err)
	}
	w.otherShared = e.createShared(w.stranger, w.other, "Their team")
	return w
}

func exampleBody(t *testing.T, name string) map[string]any {
	t.Helper()
	return map[string]any{"name": name, "definition": example(t)}
}

func (w *world) create(u *user, s access.Space, name string) App {
	w.e.t.Helper()
	rec := u.do(http.MethodPost, appsPath(s, ""), exampleBody(w.e.t, name))
	expect(w.e.t, rec, http.StatusCreated)
	a := w.publish(u, s, decode[App](w.e.t, rec))
	// The tests that count versions count from the app as it was made, live: publishing it was
	// only their setup.
	if _, err := w.e.db.Exec(`UPDATE apps SET version = 1 WHERE id = ?`, a.ID); err != nil {
		w.e.t.Fatal(err)
	}
	a.Version = 1
	return a
}

// draft creates an app and leaves it as a draft: it has no release, so it can't be launched.
func (w *world) draft(u *user, s access.Space, name string) App {
	w.e.t.Helper()
	rec := u.do(http.MethodPost, appsPath(s, ""), exampleBody(w.e.t, name))
	expect(w.e.t, rec, http.StatusCreated)
	return decode[App](w.e.t, rec)
}

// publish publishes a's draft as u, who must be allowed to, and returns the app as it is then.
func (w *world) publish(u *user, s access.Space, a App) App {
	w.e.t.Helper()
	rec := u.do(http.MethodPost, appPath(s, a.ID, "/publish"), map[string]any{"version": a.Version, "acknowledge": []SchemaChange{}})
	expect(w.e.t, rec, http.StatusOK)
	return decode[App](w.e.t, rec)
}

// action is what the screen sends for event name with ctx.
func action(a App, name string, ctx map[string]any) ClientAction {
	return ClientAction{Name: name, SurfaceID: SurfaceID(a.ID), SourceComponentID: "add", Context: ctx}
}

func (w *world) add(u *user, s access.Space, a App, description, amount string) {
	w.e.t.Helper()
	expect(w.e.t, u.do(http.MethodPost, appPath(s, a.ID, "/actions"),
		action(a, "add_expense", map[string]any{"description": description, "amount": amount})), http.StatusOK)
}

// items reads the rows a launched app shows.
func (w *world) items(u *user, s access.Space, a App) []map[string]any {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil)
	expect(w.e.t, rec, http.StatusOK)
	msgs := decode[[]struct {
		UpdateDataModel *struct {
			Value struct {
				Items []map[string]any `json:"items"`
			} `json:"value"`
		} `json:"updateDataModel"`
	}](w.e.t, rec)
	for _, m := range msgs {
		if m.UpdateDataModel != nil {
			return m.UpdateDataModel.Value.Items
		}
	}
	w.e.t.Fatal("no data model in the surface")
	return nil
}

// in returns s as addressed through workspace ws, which may not be its own.
func in(ws int64, s access.Space) access.Space {
	s.WorkspaceID = ws
	return s
}

func TestAppLifecycle(t *testing.T) {
	w := newWorld(t)
	a, s := w.aliceApp, w.alicePrivate
	if a.CreatedBy == nil || a.CreatedBy.ID != w.alice.id || a.CreatedBy.Agent || a.Version != 1 {
		t.Fatalf("created = %+v", a)
	}

	// Launch: the screen with its rows, each with its ID.
	items := w.items(w.alice, s, a)
	if len(items) != 1 || items[0]["description"] != "alice-secret" {
		t.Fatalf("items = %v", items)
	}
	// Delete it from the screen, add two more.
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "delete_expense", map[string]any{"id": items[0]["id"]})), http.StatusOK)
	w.add(w.alice, s, a, "Coffee", "4.5")
	rec := w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add_expense", map[string]any{"description": "Lunch", "amount": "12.25"}))
	expect(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"total":"16.75"`) {
		t.Fatalf("after adding: %s", rec.Body.String())
	}

	// Rename.
	rec = w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 1, "name": "Spending"})
	expect(t, rec, http.StatusOK)
	a = decode[App](t, rec)
	if a.Name != "Spending" || a.Version != 2 || a.Rows != 2 {
		t.Fatalf("renamed = %+v", a)
	}
	// A stale version changes nothing, and says what is current.
	rec = w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 1, "name": "Stale"})
	expect(t, rec, http.StatusConflict)
	if reply := decode[struct {
		Code    string
		Current App
	}](t, rec); reply.Code != "app_changed" || reply.Current.Name != "Spending" {
		t.Fatalf("409 reply = %+v", reply)
	}

	// Offline: it can't be launched and its actions don't run, but its rows are kept.
	rec = w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 2, "archived": true})
	expect(t, rec, http.StatusOK)
	a = decode[App](t, rec)
	if a.ArchivedAt == nil {
		t.Fatal("not archived")
	}
	expect(t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil), http.StatusConflict)
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add_expense", map[string]any{"description": "x", "amount": "1"})), http.StatusConflict)
	list := decode[struct{ Apps []App }](t, w.alice.do(http.MethodGet, appsPath(s, ""), nil)).Apps
	if len(list) != 0 {
		t.Fatalf("an offline app is listed: %+v", list)
	}
	list = decode[struct{ Apps []App }](t, w.alice.do(http.MethodGet, appsPath(s, "?archived=true"), nil)).Apps
	if len(list) != 1 || list[0].Rows != 2 || list[0].Draft != nil || list[0].Live == nil || list[0].Live.Definition != nil {
		t.Fatalf("with archived = %+v", list)
	}

	// Restored, it launches with the same rows.
	expect(t, w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": a.Version, "archived": false}), http.StatusOK)
	if items := w.items(w.alice, s, a); len(items) != 2 || items[0]["description"] != "Coffee" || items[1]["description"] != "Lunch" {
		t.Fatalf("items after restoring = %v", items)
	}
}

func TestEveryRouteRefusesOutsiders(t *testing.T) {
	w := newWorld(t)
	type route struct {
		method, path string
		body         any
	}
	routes := func(s access.Space, a App) []route {
		return []route{
			{http.MethodGet, appsPath(s, ""), nil},
			{http.MethodPost, appsPath(s, ""), exampleBody(t, "Intruder")},
			{http.MethodGet, appPath(s, a.ID, ""), nil},
			{http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": a.Version, "name": "Mine now"}},
			{http.MethodPost, appPath(s, a.ID, "/move"), map[string]any{"version": a.Version, "space_id": s.ID}},
			{http.MethodGet, appPath(s, a.ID, "/surface"), nil},
			{http.MethodGet, appPath(s, a.ID, "/records/query"), nil},
			{http.MethodGet, appPath(s, a.ID, "/offline"), nil},
			{http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add_expense", map[string]any{"description": "x", "amount": "1"})},
			{http.MethodGet, appPath(s, a.ID, "/roles"), nil},
			{http.MethodPut, appPath(s, a.ID, "/roles"), map[string]any{"grants": []any{}}},
			{http.MethodPost, appPath(s, a.ID, "/duplicate"), map[string]any{}},
			{http.MethodPost, appPath(s, a.ID, "/effect-board"), map[string]any{"version": a.Version, "space_id": s.ID, "board_id": 1}},
			{http.MethodPost, installPath(s, "expense-tracker"), map[string]any{}},
		}
	}
	cases := []struct {
		name   string
		u      *user
		s      access.Space
		a      App
		own    bool // the space is the user's: listing it and adding to it are theirs
		status int
	}{
		// Someone else's private space: not found, admins included.
		{"bob in alice's private space", w.bob, w.alicePrivate, w.aliceApp, false, http.StatusNotFound},
		{"the admin in alice's private space", w.owner, w.alicePrivate, w.aliceApp, false, http.StatusNotFound},
		// A shared space they aren't a member of.
		{"mallory in the team space", w.mallory, w.shared, w.sharedApp, false, http.StatusNotFound},
		// Someone who left the workspace, and their own workspace's address for it.
		{"the stranger in the team space", w.stranger, w.shared, w.sharedApp, false, http.StatusNotFound},
		{"the team space through another workspace", w.stranger, in(w.other, w.shared), w.sharedApp, false, http.StatusNotFound},
		// Their own space, but someone else's app in the path: an app is found only in its space.
		{"alice's app through bob's space", w.bob, w.bobPrivate, w.aliceApp, true, http.StatusNotFound},
		{"the team app through alice's space", w.alice, w.alicePrivate, w.sharedApp, true, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, r := range routes(c.s, c.a) {
				if c.own && (r.path == appsPath(c.s, "") || r.path == installPath(c.s, "expense-tracker")) {
					continue
				}
				if rec := c.u.do(r.method, r.path, r.body); rec.Code != c.status {
					t.Errorf("%s %s = %d, want %d: %s", r.method, r.path, rec.Code, c.status, rec.Body.String())
				}
			}
		})
	}

	// Nothing changed for anyone.
	for _, check := range []struct {
		u    *user
		s    access.Space
		a    App
		want string
	}{{w.alice, w.alicePrivate, w.aliceApp, "alice-secret"}, {w.bob, w.bobPrivate, w.bobApp, "bob-secret"}, {w.alice, w.shared, w.sharedApp, "team-lunch"}} {
		items := w.items(check.u, check.s, check.a)
		if len(items) != 1 || items[0]["description"] != check.want {
			t.Fatalf("%s's rows = %v", check.a.Name, items)
		}
		got := decode[App](t, check.u.do(http.MethodGet, appPath(check.s, check.a.ID, ""), nil))
		if got.Name != check.a.Name || got.Version != check.a.Version {
			t.Fatalf("%s changed: %+v", check.a.Name, got)
		}
	}
	// Lists show only the space's own apps.
	if list := decode[struct{ Apps []App }](t, w.bob.do(http.MethodGet, appsPath(w.bobPrivate, ""), nil)).Apps; len(list) != 1 || list[0].ID != w.bobApp.ID {
		t.Fatalf("bob's apps = %+v", list)
	}
}

func TestViewersLaunchButDontChange(t *testing.T) {
	w := newWorld(t)
	a, s := w.sharedApp, w.shared
	// Bob, a viewer of the team space, sees its app and its rows.
	if items := w.items(w.bob, s, a); len(items) != 1 || items[0]["description"] != "team-lunch" {
		t.Fatalf("bob sees %v", items)
	}
	expect(t, w.bob.do(http.MethodGet, appPath(s, a.ID, ""), nil), http.StatusOK)
	// But changes nothing: not its rows, not the app, and adds no app.
	expect(t, w.bob.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add_expense", map[string]any{"description": "x", "amount": "1"})), http.StatusForbidden)
	expect(t, w.bob.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": a.Version, "archived": true}), http.StatusForbidden)
	expect(t, w.bob.do(http.MethodPost, appsPath(s, ""), exampleBody(t, "Bob's")), http.StatusForbidden)
	// The editors do: the owner adds a row alice then sees.
	w.add(w.owner, s, a, "owner-row", "5")
	if items := w.items(w.alice, s, a); len(items) != 2 {
		t.Fatalf("alice sees %v", items)
	}

	// An archived space's apps launch, read-only, for everyone.
	if items := w.items(w.alice, w.old, w.oldApp); len(items) != 1 {
		t.Fatalf("old rows = %v", items)
	}
	rec := w.alice.do(http.MethodPost, appPath(w.old, w.oldApp.ID, "/actions"), action(w.oldApp, "add_expense", map[string]any{"description": "x", "amount": "1"}))
	expect(t, rec, http.StatusForbidden)
	expect(t, w.alice.do(http.MethodPatch, appPath(w.old, w.oldApp.ID, ""), map[string]any{"version": 1, "name": "x"}), http.StatusForbidden)
}

func TestMoveSharesAnAppWithItsRows(t *testing.T) {
	w := newWorld(t)
	a := w.aliceApp
	move := func(u *user, from access.Space, to int64, version int64) *httptest.ResponseRecorder {
		return u.do(http.MethodPost, appPath(from, a.ID, "/move"), map[string]any{"version": version, "space_id": to})
	}
	// Not to a space alice may only read (old is archived), nor one she can't see.
	expect(t, move(w.alice, w.alicePrivate, w.old.ID, 1), http.StatusForbidden)
	expect(t, move(w.alice, w.alicePrivate, w.bobPrivate.ID, 1), http.StatusNotFound)
	expect(t, move(w.alice, w.alicePrivate, w.otherShared.ID, 1), http.StatusNotFound)
	// Nor to her own private space of another workspace: an app stays in its workspace.
	if _, err := w.e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, w.other, w.alice.id); err != nil {
		t.Fatal(err)
	}
	expect(t, move(w.alice, w.alicePrivate, w.e.private(w.alice, w.other).ID, 1), http.StatusNotFound)
	// Bob, a viewer, can't move the team's app to his private space.
	expect(t, w.bob.do(http.MethodPost, appPath(w.shared, w.sharedApp.ID, "/move"), map[string]any{"version": 1, "space_id": w.bobPrivate.ID}), http.StatusForbidden)

	// Not to the space it's in: nothing would move, so nothing changes, its version included.
	expect(t, move(w.alice, w.alicePrivate, w.alicePrivate.ID, 1), http.StatusBadRequest)

	// To the team space: bob now sees it, with its row; it's no longer in alice's space.
	expect(t, w.bob.do(http.MethodGet, appPath(w.shared, a.ID, ""), nil), http.StatusNotFound)
	rec := move(w.alice, w.alicePrivate, w.shared.ID, 1)
	expect(t, rec, http.StatusOK)
	if moved := decode[App](t, rec); moved.SpaceID != w.shared.ID || moved.Version != 2 {
		t.Fatalf("moved = %+v", moved)
	}
	if items := w.items(w.bob, w.shared, a); len(items) != 1 || items[0]["description"] != "alice-secret" {
		t.Fatalf("bob sees %v", items)
	}
	expect(t, w.alice.do(http.MethodGet, appPath(w.alicePrivate, a.ID, ""), nil), http.StatusNotFound)
}

func TestSavedAppsAreCheckedOnTheServer(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	withComponent := func(c map[string]any) map[string]any {
		def := example(t)
		raw, _ := json.Marshal(c)
		def.Components = append(def.Components, raw)
		return map[string]any{"name": "Bad", "definition": def}
	}
	for name, body := range map[string]any{
		"unknown component": withComponent(map[string]any{"id": "x", "component": "Script", "src": "https://elsewhere.example/x.js"}),
		"bad props":         withComponent(map[string]any{"id": "x", "component": "Text", "text": "x", "style": "color: red"}),
		"bad prop value":    withComponent(map[string]any{"id": "x", "component": "Divider", "axis": "diagonal"}),
		"no definition":     map[string]any{"name": "Empty"},
		"unknown key":       map[string]any{"name": "x", "definition": example(t), "script": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := w.alice.do(http.MethodPost, appsPath(s, ""), body)
			expect(t, rec, http.StatusBadRequest)
		})
	}
	// A definition update is checked the same way.
	rec := w.alice.do(http.MethodPatch, appPath(s, w.aliceApp.ID, ""), map[string]any{"version": 1,
		"definition": withComponent(map[string]any{"id": "x", "component": "Image", "url": "https://elsewhere.example/p.png"})["definition"]})
	expect(t, rec, http.StatusBadRequest)

	// Rows are checked against the schema: the screen can't store what the collection refuses.
	a := w.aliceApp
	// Each with the code and params the web app says it with.
	for name, tt := range map[string]struct {
		ctx    map[string]any
		code   string
		params map[string]any
	}{
		"missing required": {map[string]any{"amount": "1"}, "app_field_required", map[string]any{"field": "description"}},
		"not a number":     {map[string]any{"description": "x", "amount": "lots"}, "app_field_not_number", map[string]any{"field": "amount"}},
		"too long": {map[string]any{"description": strings.Repeat("x", 201), "amount": "1"}, "app_field_too_long",
			map[string]any{"field": "description", "max": float64(200)}},
		"undeclared field": {map[string]any{"description": "x", "amount": "1", "owner_id": "1"}, "bad_request", nil},
	} {
		t.Run(name, func(t *testing.T) {
			rec := w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add_expense", tt.ctx))
			expect(t, rec, http.StatusUnprocessableEntity)
			reply := decode[struct {
				Code   string
				Params map[string]any
			}](t, rec)
			if reply.Code != tt.code || !reflect.DeepEqual(reply.Params, tt.params) {
				t.Fatalf("reply = %+v, want %s %v", reply, tt.code, tt.params)
			}
		})
	}
	// Another app's row can't be deleted through this one.
	bobRow := w.items(w.bob, w.bobPrivate, w.bobApp)[0]["id"]
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "delete_expense", map[string]any{"id": bobRow})), http.StatusUnprocessableEntity)
	if items := w.items(w.bob, w.bobPrivate, w.bobApp); len(items) != 1 {
		t.Fatalf("bob's rows = %v", items)
	}

	// A new collection the rows don't fit is a draft like any other: saving it is checked against
	// the catalog, and the rows are checked when it is published (release_test.go).
	def := example(t)
	def.Collection.Fields = append(def.Collection.Fields, Field{Name: "category", Type: TypeString, Required: true})
	rec = w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 1, "definition": def})
	expect(t, rec, http.StatusOK)
	// The catalog is checked at once.
	bad := example(t)
	bad.Components = append(bad.Components, json.RawMessage(`{"id":"pic","component":"Image","url":"https://example.com/x.png"}`))
	expect(t, w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 2, "definition": bad}), http.StatusBadRequest)
}

func TestRoutesNeedASessionAndSameOrigin(t *testing.T) {
	w := newWorld(t)
	s, a := w.alicePrivate, w.aliceApp
	anonymous := &user{e: w.e}
	for _, r := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, appsPath(s, ""), nil},
		{http.MethodPost, appsPath(s, ""), exampleBody(t, "x")},
		{http.MethodGet, appPath(s, a.ID, ""), nil},
		{http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 1, "name": "x"}},
		{http.MethodPost, appPath(s, a.ID, "/move"), map[string]any{"version": 1, "space_id": s.ID}},
		{http.MethodGet, appPath(s, a.ID, "/surface"), nil},
		{http.MethodPost, appPath(s, a.ID, "/actions"), action(a, "add_expense", map[string]any{"description": "x", "amount": "1"})},
		{http.MethodGet, appPath(s, a.ID, "/roles"), nil},
		{http.MethodPut, appPath(s, a.ID, "/roles"), map[string]any{"grants": []any{}}},
	} {
		if rec := anonymous.do(r.method, r.path, r.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("without a session, %s %s = %d, want 401", r.method, r.path, rec.Code)
		}
		if r.method == http.MethodGet {
			continue
		}
		// Alice's own session, from another site, or as a form: refused before any handler.
		raw, _ := json.Marshal(r.body)
		for _, attack := range []struct {
			site, contentType string
			status            int
		}{
			{"cross-site", "application/json", http.StatusForbidden},
			{"same-origin", "text/plain", http.StatusUnsupportedMediaType},
			{"same-origin", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		} {
			req := httptest.NewRequest(r.method, r.path, bytes.NewReader(raw))
			req.Header.Set("Content-Type", attack.contentType)
			req.Header.Set("Sec-Fetch-Site", attack.site)
			req.AddCookie(w.alice.cookie)
			rec := httptest.NewRecorder()
			w.e.h.ServeHTTP(rec, req)
			if rec.Code != attack.status {
				t.Errorf("%s %s (%s, %s) = %d, want %d", r.method, r.path, attack.site, attack.contentType, rec.Code, attack.status)
			}
		}
	}
	// Nothing changed.
	got := decode[App](t, w.alice.do(http.MethodGet, appPath(s, a.ID, ""), nil))
	if got.Name != a.Name || got.Version != 1 || got.Rows != 1 || got.SpaceID != s.ID {
		t.Fatalf("app = %+v", got)
	}
	var apps int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM apps WHERE space_id = ?`, s.ID).Scan(&apps); err != nil || apps != 1 {
		t.Fatalf("apps in alice's space = %d, %v", apps, err)
	}
}

func TestAppsCantMoveAcrossWorkspaces(t *testing.T) {
	w := newWorld(t)
	_, err := w.e.db.Exec(`UPDATE apps SET space_id = ? WHERE id = ?`, w.otherShared.ID, w.aliceApp.ID)
	if err == nil || !strings.Contains(err.Error(), "another workspace") {
		t.Fatalf("err = %v", err)
	}
}

func TestEveryWriteChecksAccessAgainUnderTheWriteLock(t *testing.T) {
	ctx := context.Background()
	name := "Renamed"
	for op, write := range map[string]func(w *world, team access.Space) error{
		"create": func(w *world, team access.Space) error {
			_, err := w.e.s.createApp(ctx, team, Actor{UserID: w.alice.id}, NewApp{Name: "New", Definition: example(t)})
			return err
		},
		"update": func(w *world, team access.Space) error {
			_, err := w.e.s.updateApp(ctx, team, Actor{UserID: w.alice.id}, w.sharedApp.ID, Changes{Version: w.sharedApp.Version, Name: &name})
			return err
		},
		"act": func(w *world, team access.Space) error {
			_, err := w.e.s.act(ctx, team, Actor{UserID: w.alice.id}, w.sharedApp.ID,
				action(w.sharedApp, "add_expense", map[string]any{"description": "late", "amount": "1"}))
			return err
		},
	} {
		t.Run(op, func(t *testing.T) {
			w := newWorld(t)
			// Alice's view of the team space as a request checked it: an editor. Then, before
			// the write runs, she becomes a viewer.
			team, err := w.e.s.access.Check(ctx, w.alice.id, w.shared.ID)
			if err != nil || !team.Access.Allows(access.Write) {
				t.Fatalf("team space for alice: %+v, %v", team, err)
			}
			if _, err := w.e.db.Exec(`UPDATE space_members SET role = 'viewer' WHERE space_id = ? AND user_id = ?`, w.shared.ID, w.alice.id); err != nil {
				t.Fatal(err)
			}
			if err := write(w, team); !errors.Is(err, ErrReadOnly) {
				t.Fatalf("%s with a stale snapshot = %v, want ErrReadOnly", op, err)
			}
		})
	}
}

func TestMoveChecksAccessAgainUnderTheWriteLock(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	// Alice's view of the team space, as a request checked it: an editor.
	to, err := w.e.s.access.Check(ctx, w.alice.id, w.shared.ID)
	if err != nil || !to.Access.Allows(access.Write) {
		t.Fatalf("team space for alice: %+v, %v", to, err)
	}
	from, err := w.e.s.access.Check(ctx, w.alice.id, w.alicePrivate.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Then, before the move runs, she becomes a viewer of it.
	if _, err := w.e.db.Exec(`UPDATE space_members SET role = 'viewer' WHERE space_id = ? AND user_id = ?`, w.shared.ID, w.alice.id); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.s.moveApp(ctx, from, Actor{UserID: w.alice.id}, w.aliceApp.ID, 1, to); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("move with a stale snapshot = %v, want ErrReadOnly", err)
	}
	expect(t, w.alice.do(http.MethodGet, appPath(w.alicePrivate, w.aliceApp.ID, ""), nil), http.StatusOK)
}

func TestDataSizeIsCountedInBytes(t *testing.T) {
	w := newWorld(t)
	a := w.aliceApp
	// One row of 2.5 million characters that are 2 bytes each: 5 MB, over MaxDataSize, though
	// under it counted in characters.
	big, err := json.Marshal(map[string]any{"description": strings.Repeat("é", 5<<19), "amount": 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`INSERT INTO app_rows (app_id, data, created_by, updated_by) VALUES (?, ?, ?, ?)`,
		a.ID, string(big), w.alice.id, w.alice.id); err != nil {
		t.Fatal(err)
	}
	rec := w.alice.do(http.MethodPost, appPath(w.alicePrivate, a.ID, "/actions"),
		action(a, "add_expense", map[string]any{"description": "one more", "amount": "1"}))
	expect(t, rec, http.StatusConflict)
	if reply := decode[struct{ Error, Code string }](t, rec); !strings.Contains(reply.Error, "MiB") || reply.Code != "app_full" {
		t.Fatalf("body %s", rec.Body)
	}
}
