package access

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/db"
)

var setupToken = auth.NewSetupToken()

const fakePassword = "fake-test-password"

type env struct {
	t  *testing.T
	db *sql.DB
	c  *Checker
	h  http.Handler
}

func newEnv(t *testing.T) *env {
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
	accounts := auth.New(sqlDB, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
	c := New(sqlDB, accounts)
	mux := http.NewServeMux()
	accounts.Routes(mux)
	c.Routes(mux)
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := SpaceFrom(r.Context())
		if !ok {
			t.Error("probe: no space on the context")
		}
		writeJSON(w, http.StatusOK, s)
	})
	mux.Handle("GET /api/workspaces/{workspace}/spaces/{space}/probe", c.RequireSpace(Read, probe))
	mux.Handle("POST /api/workspaces/{workspace}/spaces/{space}/probe", c.RequireSpace(Write, probe))
	return &env{t: t, db: sqlDB, c: c, h: auth.CSRF(mux)}
}

type user struct {
	e      *env
	id     int64
	cookie *http.Cookie
}

func (u *user) do(method, path string, body any) *httptest.ResponseRecorder {
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

func (e *env) join(admin *user, workspace int64, name string, role auth.Role) *user {
	e.t.Helper()
	email := strings.ToLower(name) + "@example.com"
	rec := admin.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", workspace),
		map[string]string{"email": email, "role": string(role)})
	expect(e.t, rec, http.StatusCreated)
	token := decode[struct{ Token string }](e.t, rec).Token
	u := &user{e: e}
	rec = u.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token, "name": name, "password": fakePassword})
	expect(e.t, rec, http.StatusCreated)
	u.id = decode[meReply](e.t, rec).User.ID
	return u
}

func (e *env) addWorkspace(admin *user, name string) int64 {
	e.t.Helper()
	res, err := e.db.Exec(`INSERT INTO workspaces (name) VALUES (?)`, name)
	if err != nil {
		e.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, id, admin.id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) createShared(admin *user, workspace int64, name string) Space {
	e.t.Helper()
	rec := admin.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces", workspace), map[string]string{"name": name})
	expect(e.t, rec, http.StatusCreated)
	return decode[Space](e.t, rec)
}

func (e *env) addMember(admin *user, s Space, u *user, role Role) {
	e.t.Helper()
	rec := admin.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members", s.WorkspaceID, s.ID),
		map[string]any{"user_id": u.id, "role": role})
	expect(e.t, rec, http.StatusNoContent)
}

func (e *env) spaces(u *user, workspace int64) []Space {
	e.t.Helper()
	rec := u.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces", workspace), nil)
	expect(e.t, rec, http.StatusOK)
	return decode[struct{ Spaces []Space }](e.t, rec).Spaces
}

func (e *env) private(u *user, workspace int64) Space {
	e.t.Helper()
	for _, s := range e.spaces(u, workspace) {
		if s.Kind == Private {
			return s
		}
	}
	e.t.Fatalf("user %d has no private space in workspace %d", u.id, workspace)
	return Space{}
}

type world struct {
	e                                           *env
	ws, other                                   int64
	owner, admin2, alice, bob, mallory, gone    *user
	stranger                                    *user
	shared, archived, otherShared, otherPrivate Space
	ownerPrivate, alicePrivate, gonePrivate     Space
}

func newWorld(t *testing.T) *world {
	t.Helper()
	e := newEnv(t)
	w := &world{e: e}
	w.owner, w.ws = e.setup()
	w.admin2 = e.join(w.owner, w.ws, "Admin2", auth.RoleAdmin)
	w.alice = e.join(w.owner, w.ws, "Alice", auth.RoleMember)
	w.bob = e.join(w.owner, w.ws, "Bob", auth.RoleMember)
	w.mallory = e.join(w.owner, w.ws, "Mallory", auth.RoleMember)
	w.gone = e.join(w.owner, w.ws, "Gone", auth.RoleMember)

	w.shared = e.createShared(w.owner, w.ws, "Team")
	e.addMember(w.owner, w.shared, w.alice, RoleEditor)
	e.addMember(w.owner, w.shared, w.bob, RoleViewer)
	e.addMember(w.owner, w.shared, w.gone, RoleEditor)

	w.archived = e.createShared(w.owner, w.ws, "Old")
	e.addMember(w.owner, w.archived, w.alice, RoleEditor)
	expect(t, w.owner.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/spaces/%d", w.ws, w.archived.ID),
		map[string]bool{"archived": true}), http.StatusOK)

	w.ownerPrivate = e.private(w.owner, w.ws)
	w.alicePrivate = e.private(w.alice, w.ws)
	w.gonePrivate = e.private(w.gone, w.ws)
	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", w.ws, w.gone.id), nil),
		http.StatusNoContent)

	w.stranger = e.join(w.owner, w.ws, "Stranger", auth.RoleMember)
	w.other = e.addWorkspace(w.stranger, "Elsewhere")
	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", w.ws, w.stranger.id), nil),
		http.StatusNoContent)
	w.otherShared = e.createShared(w.stranger, w.other, "Their team")
	w.otherPrivate = e.private(w.stranger, w.other)
	return w
}

func memberPath(u *user) string { return fmt.Sprintf("/members/%d", u.id) }

func spacePath(s Space, rest string) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d%s", s.WorkspaceID, s.ID, rest)
}

func in(ws int64, s Space) Space {
	s.WorkspaceID = ws
	return s
}

func TestEveryMemberHasAPrivateSpace(t *testing.T) {
	w := newWorld(t)
	for name, u := range map[string]*user{"owner at setup": w.owner, "invited": w.alice} {
		got := w.e.private(u, w.ws)
		if got.Name != "Private" || got.Role != RoleOwner || got.Access != Write || got.WorkspaceID != w.ws {
			t.Errorf("%s: private space = %+v", name, got)
		}
	}
	if w.otherPrivate.Role != RoleOwner {
		t.Errorf("private space of a new workspace = %+v", w.otherPrivate)
	}

	rec := w.stranger.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", w.other),
		map[string]string{"email": "alice@example.com"})
	expect(t, rec, http.StatusCreated)
	expect(t, w.alice.do(http.MethodPost, "/api/invites/accept",
		map[string]string{"token": decode[struct{ Token string }](t, rec).Token}), http.StatusOK)
	if got := w.e.private(w.alice, w.other); got.ID == w.alicePrivate.ID {
		t.Errorf("alice's private space in a second workspace is her first one: %+v", got)
	}

	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM spaces WHERE kind = 'private' AND owner_id = ?`, w.alice.id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("alice has %d private spaces, want one per workspace (2)", n)
	}
}

func TestListShowsOnlyMySpaces(t *testing.T) {
	w := newWorld(t)
	ids := func(spaces []Space) []int64 {
		var out []int64
		for _, s := range spaces {
			out = append(out, s.ID)
		}
		return out
	}
	cases := []struct {
		who  *user
		want []int64
	}{
		{w.owner, []int64{w.ownerPrivate.ID, w.archived.ID, w.shared.ID}},
		{w.alice, []int64{w.alicePrivate.ID, w.archived.ID, w.shared.ID}},
		{w.bob, []int64{w.e.private(w.bob, w.ws).ID, w.shared.ID}},
		{w.mallory, []int64{w.e.private(w.mallory, w.ws).ID}},
		{w.admin2, []int64{w.e.private(w.admin2, w.ws).ID}},
	}
	for _, tc := range cases {
		if got := ids(w.e.spaces(tc.who, w.ws)); !slices.Equal(got, tc.want) {
			t.Errorf("user %d lists %v, want %v", tc.who.id, got, tc.want)
		}
	}

	rec := w.admin2.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces?all=true", w.ws), nil)
	expect(t, rec, http.StatusOK)
	all := decode[struct{ Spaces []Space }](t, rec).Spaces
	if got, want := ids(all), []int64{w.e.private(w.admin2, w.ws).ID, w.archived.ID, w.shared.ID}; !slices.Equal(got, want) {
		t.Errorf("admin2 lists %v with all=true, want %v (no one else's private space)", got, want)
	}
	for _, s := range all[1:] {
		if s.Access != None || s.Role != "" {
			t.Errorf("admin2 sees %+v, want no role and access none", s)
		}
	}
	expect(t, w.mallory.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces?all=true", w.ws), nil), http.StatusForbidden)
	expect(t, w.owner.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces?all=yes", w.ws), nil), http.StatusBadRequest)

	for _, u := range []*user{w.gone, w.stranger} {
		expect(t, u.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces", w.ws), nil), http.StatusNotFound)
	}
}

func TestAccessFunctions(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	cases := []struct {
		name        string
		who         *user
		space       Space
		read, write bool
	}{
		{"own private space", w.alice, w.alicePrivate, true, true},
		{"another member's private space", w.bob, w.alicePrivate, false, false},
		{"an admin, another member's private space", w.owner, w.alicePrivate, false, false},
		{"editor", w.alice, w.shared, true, true},
		{"viewer", w.bob, w.shared, true, false},
		{"workspace member not in the shared space", w.mallory, w.shared, false, false},
		{"admin not in the shared space", w.admin2, w.shared, false, false},
		{"editor of an archived space", w.alice, w.archived, true, false},
		{"removed member, shared space", w.gone, w.shared, false, false},
		{"removed member, own private space", w.gone, w.gonePrivate, false, false},
		{"anyone, the removed member's private space", w.owner, w.gonePrivate, false, false},
		{"another workspace's shared space", w.alice, w.otherShared, false, false},
		{"another workspace's private space", w.owner, w.otherPrivate, false, false},
		{"another workspace's admin, this workspace's space", w.stranger, w.shared, false, false},
		{"a space that doesn't exist", w.owner, Space{ID: 9999}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read, err := w.e.c.CanRead(ctx, tc.who.id, tc.space.ID)
			if err != nil {
				t.Fatal(err)
			}
			write, err := w.e.c.CanWrite(ctx, tc.who.id, tc.space.ID)
			if err != nil {
				t.Fatal(err)
			}
			if read != tc.read || write != tc.write {
				t.Errorf("CanRead, CanWrite = %v, %v; want %v, %v", read, write, tc.read, tc.write)
			}
			s, err := w.e.c.Check(ctx, tc.who.id, tc.space.ID)
			switch {
			case tc.read && (err != nil || s.ID != tc.space.ID):
				t.Errorf("Check = %+v, %v; want the space", s, err)
			case !tc.read && (!errors.Is(err, ErrNotFound) || s != Space{}):
				t.Errorf("Check = %+v, %v; want ErrNotFound", s, err)
			}
		})
	}

	readable, err := w.e.c.Readable(ctx, w.bob.id, w.ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(readable) != 2 || readable[0].Kind != Private || readable[1].ID != w.shared.ID || readable[1].Access != Read {
		t.Errorf("Readable(bob) = %+v, want his private space and shared as a viewer", readable)
	}
	if readable, err := w.e.c.Readable(ctx, w.gone.id, w.ws); err != nil || len(readable) != 0 {
		t.Errorf("Readable(removed member) = %+v, %v; want nothing", readable, err)
	}
}

func TestRequireSpace(t *testing.T) {
	w := newWorld(t)
	cases := []struct {
		name        string
		who         *user
		space       Space
		read, write int
	}{
		{"own private space", w.alice, w.alicePrivate, 200, 200},
		{"another member's private space", w.bob, w.alicePrivate, 404, 404},
		{"an admin, another member's private space", w.owner, w.alicePrivate, 404, 404},
		{"editor", w.alice, w.shared, 200, 200},
		{"viewer", w.bob, w.shared, 200, 403},
		{"workspace member not in the shared space", w.mallory, w.shared, 404, 404},
		{"admin not in the shared space", w.admin2, w.shared, 404, 404},
		{"editor of an archived space", w.alice, w.archived, 200, 403},
		{"removed member, shared space", w.gone, w.shared, 404, 404},
		{"removed member, own private space", w.gone, w.gonePrivate, 404, 404},
		{"admin, the removed member's private space", w.owner, w.gonePrivate, 404, 404},
		{"another workspace's space, through this workspace", w.owner, in(w.ws, w.otherShared), 404, 404},
		{"this workspace's space, through another", w.stranger, in(w.other, w.shared), 404, 404},
		{"another workspace's space, through it, as an outsider", w.alice, w.otherShared, 404, 404},
		{"a space that doesn't exist", w.owner, in(w.ws, Space{ID: 9999}), 404, 404},
		{"a space ID that isn't a number", w.owner, Space{WorkspaceID: w.ws}, 404, 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := spacePath(tc.space, "/probe")
			if tc.space.ID == 0 {
				path = fmt.Sprintf("/api/workspaces/%d/spaces/x/probe", tc.space.WorkspaceID)
			}
			rec := tc.who.do(http.MethodGet, path, nil)
			expect(t, rec, tc.read)
			if tc.read == 200 && decode[Space](t, rec).ID != tc.space.ID {
				t.Errorf("the context holds another space: %s", rec.Body.String())
			}
			expect(t, tc.who.do(http.MethodPost, path, map[string]string{}), tc.write)
		})
	}

	anon := &user{e: w.e}
	expect(t, anon.do(http.MethodGet, spacePath(w.shared, "/probe"), nil), http.StatusUnauthorized)
}

func TestSpaceRoutes(t *testing.T) {
	w := newWorld(t)
	editor := map[string]any{"role": "editor"}
	cases := []struct {
		name   string
		who    *user
		method string
		path   string
		body   any
		want   int
	}{
		{"member creates a shared space", w.alice, "POST", fmt.Sprintf("/api/workspaces/%d/spaces", w.ws), map[string]string{"name": "x"}, 403},
		{"outsider creates a shared space", w.stranger, "POST", fmt.Sprintf("/api/workspaces/%d/spaces", w.ws), map[string]string{"name": "x"}, 404},
		{"removed member creates a shared space", w.gone, "POST", fmt.Sprintf("/api/workspaces/%d/spaces", w.ws), map[string]string{"name": "x"}, 404},
		{"admin, empty name", w.owner, "POST", fmt.Sprintf("/api/workspaces/%d/spaces", w.ws), map[string]string{"name": "  "}, 400},
		{"admin, unknown field", w.owner, "POST", fmt.Sprintf("/api/workspaces/%d/spaces", w.ws), map[string]string{"name": "x", "kind": "private"}, 400},

		{"editor reads the space", w.alice, "GET", spacePath(w.shared, ""), nil, 200},
		{"viewer reads the space", w.bob, "GET", spacePath(w.shared, ""), nil, 200},
		{"admin outside reads the shared space's details", w.admin2, "GET", spacePath(w.shared, ""), nil, 200},
		{"member outside reads the shared space", w.mallory, "GET", spacePath(w.shared, ""), nil, 404},
		{"another's private space", w.bob, "GET", spacePath(w.alicePrivate, ""), nil, 404},
		{"admin, another's private space", w.owner, "GET", spacePath(w.alicePrivate, ""), nil, 404},
		{"removed member", w.gone, "GET", spacePath(w.shared, ""), nil, 404},
		{"removed member's private space", w.owner, "GET", spacePath(w.gonePrivate, ""), nil, 404},
		{"another workspace's space through this one", w.owner, "GET", spacePath(in(w.ws, w.otherShared), ""), nil, 404},
		{"another workspace's space", w.alice, "GET", spacePath(w.otherShared, ""), nil, 404},

		{"editor renames", w.alice, "PATCH", spacePath(w.shared, ""), map[string]string{"name": "x"}, 403},
		{"viewer archives", w.bob, "PATCH", spacePath(w.shared, ""), map[string]bool{"archived": true}, 403},
		{"admin renames another's private space", w.owner, "PATCH", spacePath(w.alicePrivate, ""), map[string]string{"name": "x"}, 404},
		{"admin renames own private space", w.owner, "PATCH", spacePath(w.ownerPrivate, ""), map[string]string{"name": "x"}, 404},
		{"admin renames another workspace's space", w.owner, "PATCH", spacePath(in(w.ws, w.otherShared), ""), map[string]string{"name": "x"}, 404},
		{"admin, empty change", w.owner, "PATCH", spacePath(w.shared, ""), map[string]string{}, 400},

		{"viewer lists members", w.bob, "GET", spacePath(w.shared, "/members"), nil, 200},
		{"admin outside lists members", w.admin2, "GET", spacePath(w.shared, "/members"), nil, 200},
		{"member outside lists members", w.mallory, "GET", spacePath(w.shared, "/members"), nil, 404},
		{"removed member lists members", w.gone, "GET", spacePath(w.shared, "/members"), nil, 404},
		{"owner lists own private space's members", w.alice, "GET", spacePath(w.alicePrivate, "/members"), nil, 404},
		{"admin lists another's private space's members", w.owner, "GET", spacePath(w.alicePrivate, "/members"), nil, 404},
		{"another workspace's space's members", w.owner, "GET", spacePath(in(w.ws, w.otherShared), "/members"), nil, 404},

		{"editor adds a member", w.alice, "POST", spacePath(w.shared, "/members"), map[string]any{"user_id": w.mallory.id, "role": "viewer"}, 403},
		{"member outside adds themselves", w.mallory, "POST", spacePath(w.shared, "/members"), map[string]any{"user_id": w.mallory.id, "role": "editor"}, 403},
		{"admin adds someone to another's private space", w.owner, "POST", spacePath(w.alicePrivate, "/members"), map[string]any{"user_id": w.owner.id, "role": "editor"}, 404},
		{"admin adds someone to own private space", w.owner, "POST", spacePath(w.ownerPrivate, "/members"), map[string]any{"user_id": w.alice.id, "role": "viewer"}, 404},
		{"admin adds a removed member", w.owner, "POST", spacePath(w.shared, "/members"), map[string]any{"user_id": w.gone.id, "role": "viewer"}, 404},
		{"admin adds another workspace's user", w.owner, "POST", spacePath(w.shared, "/members"), map[string]any{"user_id": w.stranger.id, "role": "viewer"}, 404},
		{"admin adds an unknown user", w.owner, "POST", spacePath(w.shared, "/members"), map[string]any{"user_id": 9999, "role": "viewer"}, 404},
		{"admin adds a member twice", w.owner, "POST", spacePath(w.shared, "/members"), map[string]any{"user_id": w.alice.id, "role": "viewer"}, 409},
		{"admin, bad role", w.owner, "POST", spacePath(w.shared, "/members"), map[string]any{"user_id": w.mallory.id, "role": "owner"}, 400},
		{"admin adds to another workspace's space", w.owner, "POST", spacePath(in(w.ws, w.otherShared), "/members"), map[string]any{"user_id": w.alice.id, "role": "viewer"}, 404},
		{"viewer makes themselves an editor", w.bob, "PATCH", spacePath(w.shared, memberPath(w.bob)), editor, 403},
		{"admin changes a non-member", w.owner, "PATCH", spacePath(w.shared, memberPath(w.mallory)), editor, 404},
		{"admin changes a removed member", w.owner, "PATCH", spacePath(w.shared, memberPath(w.gone)), editor, 404},
		{"admin, bad user ID", w.owner, "PATCH", spacePath(w.shared, "/members/x"), editor, 404},
		{"editor removes a member", w.alice, "DELETE", spacePath(w.shared, memberPath(w.bob)), nil, 403},
		{"admin removes a non-member", w.owner, "DELETE", spacePath(w.shared, memberPath(w.mallory)), nil, 404},
		{"admin removes from another workspace's space", w.owner, "DELETE", spacePath(in(w.ws, w.otherShared), memberPath(w.stranger)), nil, 404},
		{"outsider removes a member", w.stranger, "DELETE", spacePath(w.shared, memberPath(w.bob)), nil, 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expect(t, tc.who.do(tc.method, tc.path, tc.body), tc.want)
		})
	}

	got := decode[struct{ Members []Member }](t, w.owner.do(http.MethodGet, spacePath(w.shared, "/members"), nil)).Members
	var summary []string
	for _, m := range got {
		summary = append(summary, m.Name+":"+string(m.Role))
	}
	if want := []string{"Alice:editor", "Bob:viewer", "Owner:editor"}; !slices.Equal(summary, want) {
		t.Errorf("members after the refused calls = %v, want %v", summary, want)
	}
	if s := decode[Space](t, w.owner.do(http.MethodGet, spacePath(w.shared, ""), nil)); s.Name != "Team" || s.ArchivedAt != nil {
		t.Errorf("shared space after the refused calls = %+v", s)
	}
}

func TestManagingASharedSpace(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.createShared(w.owner, w.ws, "  Launch  ")
	if s.Name != "Launch" || s.Kind != Shared || s.Role != RoleEditor || s.Access != Write || s.ArchivedAt != nil {
		t.Fatalf("created %+v, want a shared space with its creator as an editor", s)
	}

	e.addMember(w.owner, s, w.mallory, RoleViewer)
	expect(t, w.mallory.do(http.MethodGet, spacePath(s, "/probe"), nil), http.StatusOK)
	expect(t, w.mallory.do(http.MethodPost, spacePath(s, "/probe"), map[string]string{}), http.StatusForbidden)
	expect(t, w.owner.do(http.MethodPatch, spacePath(s, memberPath(w.mallory)), map[string]string{"role": "editor"}), http.StatusNoContent)
	expect(t, w.mallory.do(http.MethodPost, spacePath(s, "/probe"), map[string]string{}), http.StatusOK)

	rec := w.admin2.do(http.MethodPatch, spacePath(s, ""), map[string]string{"name": "Launch 2"})
	expect(t, rec, http.StatusOK)
	if got := decode[Space](t, rec); got.Name != "Launch 2" || got.Access != None {
		t.Errorf("renamed by an admin outside it: %+v", got)
	}
	expect(t, w.owner.do(http.MethodPatch, spacePath(s, ""), map[string]bool{"archived": true}), http.StatusOK)
	rec = w.mallory.do(http.MethodPost, spacePath(s, "/probe"), map[string]string{})
	expect(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "archived") {
		t.Errorf("refusal for an archived space = %s, want it to say so", rec.Body.String())
	}
	expect(t, w.mallory.do(http.MethodGet, spacePath(s, "/probe"), nil), http.StatusOK)
	archivedAt := *decode[Space](t, w.owner.do(http.MethodGet, spacePath(s, ""), nil)).ArchivedAt
	expect(t, w.owner.do(http.MethodPatch, spacePath(s, ""), map[string]bool{"archived": true}), http.StatusOK)
	if again := decode[Space](t, w.owner.do(http.MethodGet, spacePath(s, ""), nil)).ArchivedAt; again == nil || *again != archivedAt {
		t.Errorf("archiving twice moved archived_at from %s to %v", archivedAt, again)
	}
	expect(t, w.owner.do(http.MethodPatch, spacePath(s, ""), map[string]bool{"archived": false}), http.StatusOK)
	expect(t, w.mallory.do(http.MethodPost, spacePath(s, "/probe"), map[string]string{}), http.StatusOK)

	e.addMember(w.admin2, s, w.admin2, RoleViewer)
	expect(t, w.admin2.do(http.MethodGet, spacePath(s, "/probe"), nil), http.StatusOK)
	members := decode[struct{ Members []Member }](t, w.mallory.do(http.MethodGet, spacePath(s, "/members"), nil)).Members
	if !slices.ContainsFunc(members, func(m Member) bool { return m.ID == w.admin2.id && m.Role == RoleViewer }) {
		t.Errorf("members = %+v, want admin2 listed", members)
	}

	expect(t, w.owner.do(http.MethodDelete, spacePath(s, memberPath(w.mallory)), nil), http.StatusNoContent)
	expect(t, w.mallory.do(http.MethodGet, spacePath(s, "/probe"), nil), http.StatusNotFound)
	expect(t, w.mallory.do(http.MethodGet, spacePath(s, ""), nil), http.StatusNotFound)
}

func TestDisabledAccountHasNoAccess(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	read := func() (bool, error) {
		ok, err := w.e.c.CanRead(ctx, w.alice.id, w.shared.ID)
		return ok, err
	}
	if ok, err := read(); err != nil || !ok {
		t.Fatalf("before: CanRead = %v, %v", ok, err)
	}
	spaces := func() int {
		got, err := w.e.c.Readable(ctx, w.alice.id, w.ws)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	before := spaces()
	if before == 0 {
		t.Fatal("alice has no readable spaces to begin with")
	}
	expect(t, w.owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/disable", w.ws, w.alice.id), nil), http.StatusNoContent)
	if _, err := w.e.c.Check(ctx, w.alice.id, w.shared.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Check while disabled = %v, want ErrNotFound", err)
	} else if _, err := w.e.c.Check(ctx, w.alice.id, w.shared.ID); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Check while disabled = %v, want ErrDisabled, so that queued work can wait", err)
	}
	if _, err := w.e.c.Check(ctx, w.mallory.id, w.shared.ID); !errors.Is(err, ErrNotFound) || errors.Is(err, ErrDisabled) {
		t.Fatalf("Check by an outsider = %v, want ErrNotFound and not ErrDisabled", err)
	}
	if off, err := w.e.c.Disabled(ctx, w.alice.id); err != nil || !off {
		t.Fatalf("Disabled = %v, %v", off, err)
	}
	if ok, _ := read(); ok {
		t.Fatal("a disabled account can read a space")
	}
	if n := spaces(); n != 0 {
		t.Fatalf("a disabled account has %d readable spaces", n)
	}
	if ok, err := w.e.c.CanRead(ctx, w.bob.id, w.shared.ID); err != nil || !ok {
		t.Fatalf("bob while alice is disabled: %v, %v", ok, err)
	}
	expect(t, w.owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/enable", w.ws, w.alice.id), nil), http.StatusNoContent)
	if n := spaces(); n != before {
		t.Fatalf("after enabling: %d readable spaces, want the %d from before", n, before)
	}
}

func TestLeavingTheWorkspace(t *testing.T) {
	w := newWorld(t)
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM space_members WHERE user_id = ?`, w.gone.id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the removed member keeps %d shared-space memberships", n)
	}
	if err := w.e.db.QueryRow(`SELECT count(*) FROM spaces WHERE id = ?`, w.gonePrivate.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the removed member's private space: %d rows, %v", n, err)
	}

	rec := w.owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", w.ws), map[string]string{"email": "gone@example.com"})
	expect(t, rec, http.StatusCreated)
	expect(t, w.gone.do(http.MethodPost, "/api/invites/accept",
		map[string]string{"token": decode[struct{ Token string }](t, rec).Token}), http.StatusOK)
	spaces := w.e.spaces(w.gone, w.ws)
	if len(spaces) != 1 || spaces[0].ID != w.gonePrivate.ID {
		t.Errorf("after rejoining, spaces = %+v; want only the old private space %d", spaces, w.gonePrivate.ID)
	}
	expect(t, w.gone.do(http.MethodGet, spacePath(w.shared, "/probe"), nil), http.StatusNotFound)
}

func TestTheSchemaHoldsTheRules(t *testing.T) {
	w := newWorld(t)
	refused := []struct {
		name  string
		query string
		args  []any
	}{
		{"a member of a private space", `INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'viewer')`,
			[]any{w.alicePrivate.ID, w.bob.id}},
		{"a space member from outside the workspace", `INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'viewer')`,
			[]any{w.shared.ID, w.stranger.id}},
		{"a removed member back in a shared space", `INSERT INTO space_members (space_id, user_id, role) VALUES (?, ?, 'viewer')`,
			[]any{w.shared.ID, w.gone.id}},
		{"an unknown role", `UPDATE space_members SET role = 'admin' WHERE space_id = ?`, []any{w.shared.ID}},
		{"moving a space to another workspace", `UPDATE spaces SET workspace_id = ? WHERE id = ?`, []any{w.other, w.shared.ID}},
		{"giving a private space to someone else", `UPDATE spaces SET owner_id = ? WHERE id = ?`, []any{w.bob.id, w.alicePrivate.ID}},
		{"making a private space shared", `UPDATE spaces SET kind = 'shared', owner_id = NULL WHERE id = ?`, []any{w.alicePrivate.ID}},
		{"moving a membership to another space", `UPDATE space_members SET space_id = ? WHERE space_id = ?`, []any{w.archived.ID, w.shared.ID}},
		{"a second private space", `INSERT INTO spaces (workspace_id, kind, owner_id, name) VALUES (?, 'private', ?, 'x')`,
			[]any{w.ws, w.alice.id}},
		{"a shared space with an owner", `INSERT INTO spaces (workspace_id, kind, owner_id, name) VALUES (?, 'shared', ?, 'x')`,
			[]any{w.ws, w.alice.id}},
		{"an archived private space", `UPDATE spaces SET archived_at = 'x' WHERE id = ?`, []any{w.alicePrivate.ID}},
		{"deleting a user who has a private space", `DELETE FROM users WHERE id = ?`, []any{w.gone.id}},
	}
	for _, tc := range refused {
		if _, err := w.e.db.Exec(tc.query, tc.args...); err == nil {
			t.Errorf("%s: allowed", tc.name)
		}
	}
}
