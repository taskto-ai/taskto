package boards

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/db"
)

// Test passwords: obviously fake, and long enough for the account rules.
const fakePassword = "fake-test-password"

type env struct {
	t  *testing.T
	db *sql.DB
	s  *Service
	h  http.Handler
	// hooks run what a test sets at chosen moments of the database's use.
	hooks *db.Hooks
}

// newEnv serves the account, space and board routes, as the server does.

// setupToken is the first-run setup token the tests' server is given.
var setupToken = auth.NewSetupToken()

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	hooks := &db.Hooks{}
	sqlDB, err := db.OpenHooked(ctx, filepath.Join(t.TempDir(), db.FileName), hooks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	accounts := auth.New(sqlDB, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}, SetupToken: setupToken})
	spaces := access.New(sqlDB, accounts)
	s := New(sqlDB, spaces)
	mux := http.NewServeMux()
	accounts.Routes(mux)
	spaces.Routes(mux)
	s.Routes(mux)
	return &env{t: t, db: sqlDB, s: s, h: auth.CSRF(mux), hooks: hooks}
}

// user is a logged-in browser.
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

// setup creates the owner and their workspace.
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

// join invites a new person to workspace, as admin, and returns them logged in.
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

func (e *env) createShared(admin *user, workspace int64, name string) access.Space {
	e.t.Helper()
	rec := admin.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces", workspace), map[string]string{"name": name})
	expect(e.t, rec, http.StatusCreated)
	return decode[access.Space](e.t, rec)
}

func (e *env) addMember(admin *user, s access.Space, u *user, role access.Role) {
	e.t.Helper()
	rec := admin.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members", s.WorkspaceID, s.ID),
		map[string]any{"user_id": u.id, "role": role})
	expect(e.t, rec, http.StatusNoContent)
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
	e.t.Fatalf("user %d has no private space in workspace %d", u.id, workspace)
	return access.Space{}
}

func spacePath(s access.Space, rest string) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d%s", s.WorkspaceID, s.ID, rest)
}

func boardPath(s access.Space, board int64, rest string) string {
	return spacePath(s, fmt.Sprintf("/boards/%d%s", board, rest))
}

func cardPath(s access.Space, board, card int64, rest string) string {
	return boardPath(s, board, fmt.Sprintf("/cards/%d%s", card, rest))
}

// in returns s as addressed through workspace ws, which may not be its own.
func in(ws int64, s access.Space) access.Space {
	s.WorkspaceID = ws
	return s
}

// world is a workspace with people in every relation to its spaces and boards:
//
//   - owner: admin, editor of shared and archived;
//   - admin2: admin, member of no shared space;
//   - alice: editor of shared and archived;
//   - bob: viewer of shared;
//   - mallory: a member of the workspace, of no shared space;
//   - gone: was an editor of shared, then removed from the workspace;
//   - stranger: admin of another workspace, other, with its own shared space.
//
// shared has two team boards, team and team2; archived (an archived space) has one,
// old. Each has a card.
type world struct {
	e                                       *env
	ws, other                               int64
	owner, admin2, alice, bob, mallory      *user
	gone, stranger                          *user
	shared, archived, otherShared           access.Space
	alicePrivate, ownerPrivate              access.Space
	team, team2, old, alicePersonal         Board
	teamCard, team2Card, oldCard, aliceCard Card
	agentIDs                                int64 // the last agent runtime ID conversation used
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
	w.stranger = e.join(w.owner, w.ws, "Stranger", auth.RoleMember)

	w.shared = e.createShared(w.owner, w.ws, "Team")
	e.addMember(w.owner, w.shared, w.alice, access.RoleEditor)
	e.addMember(w.owner, w.shared, w.bob, access.RoleViewer)
	e.addMember(w.owner, w.shared, w.gone, access.RoleEditor)
	w.team = w.createBoard(w.alice, w.shared, "Launch")
	w.team2 = w.createBoard(w.alice, w.shared, "Ops")
	w.teamCard = w.createCard(w.alice, w.shared, w.team, map[string]any{"title": "Team card"})
	w.team2Card = w.createCard(w.alice, w.shared, w.team2, map[string]any{"title": "Ops card"})

	w.archived = e.createShared(w.owner, w.ws, "Old")
	e.addMember(w.owner, w.archived, w.alice, access.RoleEditor)
	w.old = w.createBoard(w.alice, w.archived, "Old board")
	w.oldCard = w.createCard(w.alice, w.archived, w.old, map[string]any{"title": "Old card"})
	expect(t, w.owner.do(http.MethodPatch, spacePath(w.archived, ""), map[string]bool{"archived": true}), http.StatusOK)

	w.alicePrivate = e.private(w.alice, w.ws)
	w.ownerPrivate = e.private(w.owner, w.ws)
	w.alicePersonal = w.boards(w.alice, w.alicePrivate)[0]
	w.aliceCard = w.createCard(w.alice, w.alicePrivate, w.alicePersonal, map[string]any{"title": "Alice's card"})

	for _, u := range []*user{w.gone, w.stranger} {
		expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", w.ws, u.id), nil),
			http.StatusNoContent)
	}
	res, err := e.db.Exec(`INSERT INTO workspaces (name) VALUES ('Elsewhere')`)
	if err != nil {
		t.Fatal(err)
	}
	w.other, _ = res.LastInsertId()
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, w.other, w.stranger.id); err != nil {
		t.Fatal(err)
	}
	w.otherShared = e.createShared(w.stranger, w.other, "Their team")
	return w
}

func (w *world) boards(u *user, s access.Space) []Board {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, spacePath(s, "/boards"), nil)
	expect(w.e.t, rec, http.StatusOK)
	return decode[struct{ Boards []Board }](w.e.t, rec).Boards
}

func (w *world) createBoard(u *user, s access.Space, name string) Board {
	w.e.t.Helper()
	rec := u.do(http.MethodPost, spacePath(s, "/boards"), map[string]string{"name": name})
	expect(w.e.t, rec, http.StatusCreated)
	return decode[Board](w.e.t, rec)
}

func (w *world) createCard(u *user, s access.Space, b Board, body map[string]any) Card {
	w.e.t.Helper()
	rec := u.do(http.MethodPost, boardPath(s, b.ID, "/cards"), body)
	expect(w.e.t, rec, http.StatusCreated)
	return decode[Card](w.e.t, rec)
}

func (w *world) card(u *user, s access.Space, c Card) Thread {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, cardPath(s, c.BoardID, c.ID, ""), nil)
	expect(w.e.t, rec, http.StatusOK)
	return decode[Thread](w.e.t, rec)
}

// cards lists a board's cards with a query string ("" or "?column=todo&...").
func (w *world) cards(u *user, s access.Space, b Board, query string) []Card {
	w.e.t.Helper()
	rec := u.do(http.MethodGet, boardPath(s, b.ID, "/cards"+query), nil)
	expect(w.e.t, rec, http.StatusOK)
	return decode[struct{ Cards []Card }](w.e.t, rec).Cards
}

func titles(cards []Card) []string {
	out := []string{}
	for _, c := range cards {
		out = append(out, c.Title)
	}
	return out
}

// conversation records a conversation of u in workspace ws, as the conversation API does,
// and returns its ID as a card links it.
func (w *world) conversation(u *user, ws int64) string {
	w.e.t.Helper()
	w.agentIDs++
	var id int64
	if err := w.e.db.QueryRow(`INSERT INTO conversations (workspace_id, owner_id, agent_id) VALUES (?, ?, ?) RETURNING id`,
		ws, u.id, w.agentIDs).Scan(&id); err != nil {
		w.e.t.Fatal(err)
	}
	return strconv.FormatInt(id, 10)
}
