package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/agenttools"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/providers"
	"taskto.ai/taskto/internal/search"
	"taskto.ai/taskto/internal/serverkey"
)

// fakePassword is obviously fake, and long enough.
const fakePassword = "fake-test-password"

// stack is the whole server, as cmd/taskto puts it together, with the MCP endpoint, and
// people in it:
//
//   - workspace one: alice (admin), bob and carol. secret is a shared space only carol is
//     in; team is one alice and bob edit.
//   - workspace two: alice, alone.
type stack struct {
	t   *testing.T
	db  *sql.DB
	h   http.Handler
	dir string // the data directory
	one int64
	two int64

	alice, bob, carol  person
	secret, team       int64
	alicePrivate2      int64
	alicePersonalBoard int64
	setupToken, mcpURL string
}

type person struct {
	id      int64
	cookie  string // the browser's session
	private int64  // their private space in one
}

func newStack(t *testing.T) *stack { return newStackWith(t, nil) }

// newStackWith is newStack with the server's options changed by mod.
func newStackWith(t *testing.T, mod func(*Options)) *stack {
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
	kb := knowledge.New(dir, knowledge.Options{})
	store := providers.NewStore(sqlDB, key)
	index := search.New(sqlDB, kb, store, search.Options{})
	spaces := access.New(sqlDB, nil)
	tools := agenttools.New(agenttools.Options{
		DB: sqlDB, Key: key, Access: spaces, Boards: boards.New(sqlDB, spaces), Apps: apps.New(sqlDB, spaces),
		Knowledge: kb, Search: index,
	})
	s := &stack{t: t, db: sqlDB, dir: dir, setupToken: auth.NewSetupToken()}
	opts := Options{
		Auth:          auth.Options{InsecureCookies: true, SetupToken: s.setupToken, HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}},
		Search:        index,
		Providers:     providers.New(store, nil),
		Conversations: conversations.New(sqlDB, nil),
		Tools:         tools,
		BackupDataDir: dir,
	}
	if mod != nil {
		mod(&opts)
	}
	s.h = newHandler(t, sqlDB, opts)
	srv := httptest.NewServer(s.h)
	t.Cleanup(srv.Close)
	s.mcpURL = srv.URL + "/mcp"

	code, body := s.browser("", http.MethodPost, "/api/setup", map[string]any{
		"email": "alice@example.com", "name": "Alice", "password": fakePassword, "workspace_name": "One", "setup_token": s.setupToken,
	})
	if code != http.StatusCreated {
		t.Fatalf("setup = %d %v", code, body)
	}
	s.alice.id = id(body["user"].(map[string]any)["id"])
	s.one = id(body["workspaces"].([]any)[0].(map[string]any)["id"])
	s.bob = s.invite("bob@example.com", "Bob")
	s.carol = s.invite("carol@example.com", "Carol")
	for _, p := range []*person{&s.alice, &s.bob, &s.carol} {
		p.private = s.scalar(`SELECT id FROM spaces WHERE workspace_id = ? AND owner_id = ?`, s.one, p.id)
	}
	s.secret = s.sharedSpace("Secret", s.carol)
	s.team = s.sharedSpace("Team", s.bob)
	s.alicePersonalBoard = s.scalar(`SELECT id FROM boards WHERE space_id = ?`, s.alice.private)

	// A second workspace for alice: there is no API to make one yet.
	s.two = s.scalar(`INSERT INTO workspaces (name) VALUES ('Two') RETURNING id`)
	s.exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, s.two, s.alice.id)
	s.alicePrivate2 = s.scalar(`SELECT id FROM spaces WHERE workspace_id = ? AND owner_id = ?`, s.two, s.alice.id)
	return s
}

// invite adds a member to one, through an invite alice makes, and logs them in.
func (s *stack) invite(email, name string) person {
	s.t.Helper()
	code, inv := s.browser(s.alice.cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", s.one), map[string]any{"email": email})
	if code != http.StatusCreated {
		s.t.Fatalf("invite = %d %v", code, inv)
	}
	rec := s.request(http.MethodPost, "/api/invites/accept", map[string]any{"token": inv["token"], "name": name, "password": fakePassword}, sameOrigin(""))
	if rec.Code != http.StatusCreated {
		s.t.Fatalf("accept = %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return person{id: id(body["user"].(map[string]any)["id"]), cookie: sessionCookie(rec)}
}

// sharedSpace makes a shared space of one with editor as its only member besides alice,
// who then leaves it unless editor is bob.
func (s *stack) sharedSpace(name string, editor person) int64 {
	s.t.Helper()
	ws := fmt.Sprintf("/api/workspaces/%d/spaces", s.one)
	code, sp := s.browser(s.alice.cookie, http.MethodPost, ws, map[string]any{"name": name})
	if code != http.StatusCreated {
		s.t.Fatalf("create space = %d %v", code, sp)
	}
	spaceID := id(sp["id"])
	if code, body := s.browser(s.alice.cookie, http.MethodPost, fmt.Sprintf("%s/%d/members", ws, spaceID),
		map[string]any{"user_id": editor.id, "role": "editor"}); code != http.StatusNoContent {
		s.t.Fatalf("add member = %d %v", code, body)
	}
	if editor.id != s.bob.id {
		if code, body := s.browser(s.alice.cookie, http.MethodDelete, fmt.Sprintf("%s/%d/members/%d", ws, spaceID, s.alice.id), nil); code != http.StatusNoContent {
			s.t.Fatalf("leave space = %d %v", code, body)
		}
	}
	return spaceID
}

func (s *stack) exec(q string, args ...any) {
	s.t.Helper()
	if _, err := s.db.Exec(q, args...); err != nil {
		s.t.Fatal(err)
	}
}

func (s *stack) scalar(q string, args ...any) int64 {
	s.t.Helper()
	var v int64
	if err := s.db.QueryRow(q, args...).Scan(&v); err != nil {
		s.t.Fatal(err)
	}
	return v
}

func id(v any) int64 { return int64(v.(float64)) }

// sameOrigin is a browser's same-origin request, with its session cookie if it has one.
func sameOrigin(cookie string) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		if cookie != "" {
			r.Header.Set("Cookie", "taskto_session="+cookie)
		}
	}
}

// bearer is a client with an access token: not a browser, so no Sec-Fetch-Site or Origin.
func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func sessionCookie(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "taskto_session" {
			return c.Value
		}
	}
	return ""
}

func (s *stack) request(method, path string, body any, with ...func(*http.Request)) *httptest.ResponseRecorder {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, path, rd)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, f := range with {
		f(r)
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, r)
	return rec
}

func decode(rec *httptest.ResponseRecorder) map[string]any {
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// browser makes a same-origin request with a session cookie.
func (s *stack) browser(cookie, method, path string, body any) (int, map[string]any) {
	s.t.Helper()
	rec := s.request(method, path, body, sameOrigin(cookie))
	if s.alice.cookie == "" && path == "/api/setup" {
		s.alice.cookie = sessionCookie(rec)
	}
	return rec.Code, decode(rec)
}

// api makes a request with an access token.
func (s *stack) api(token, method, path string, body any) (int, map[string]any) {
	s.t.Helper()
	rec := s.request(method, path, body, bearer(token))
	return rec.Code, decode(rec)
}

// newToken makes an access token in workspace ws for the browser with cookie.
func (s *stack) newToken(cookie string, ws int64, name string) (string, map[string]any) {
	s.t.Helper()
	code, body := s.browser(cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/tokens", ws), map[string]any{"name": name})
	if code != http.StatusCreated {
		s.t.Fatalf("create token = %d %v", code, body)
	}
	return body["token"].(string), body
}

// tool calls a tool over MCP with token, as one stateless JSON-RPC request, and returns the
// HTTP status, whether the tool refused, and its text.
func (s *stack) tool(token, name string, args map[string]any) (int, bool, string) {
	s.t.Helper()
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(msg))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-06-18")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		return rec.Code, true, rec.Body.String()
	}
	var reply struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || reply.Error != nil || len(reply.Result.Content) != 1 {
		s.t.Fatalf("%s: unexpected reply %s", name, rec.Body)
	}
	return rec.Code, reply.Result.IsError, reply.Result.Content[0].Text
}

// toolOK is tool for a call that must succeed, decoding its result.
func (s *stack) toolOK(token, name string, args map[string]any) map[string]any {
	s.t.Helper()
	code, refused, text := s.tool(token, name, args)
	if code != http.StatusOK || refused {
		s.t.Fatalf("%s(%v) = %d %s", name, args, code, text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		s.t.Fatalf("%s: %v in %s", name, err, text)
	}
	return out
}

func (s *stack) apps(ws, space int64) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d/apps", ws, space)
}

// app makes the bundled example app in a space of one with a browser's session, with one
// row, published, and returns its ID.
func (s *stack) app(cookie string, space int64, row string) int64 {
	s.t.Helper()
	ex, err := apps.Examples()
	if err != nil {
		s.t.Fatal(err)
	}
	code, a := s.browser(cookie, http.MethodPost, s.apps(s.one, space), map[string]any{"name": "Expenses", "definition": ex[0].Definition})
	if code != http.StatusCreated {
		s.t.Fatalf("create app = %d %v", code, a)
	}
	appID := id(a["id"])
	// A new app is a draft: its owner publishes it (which makes it version 2) before it can be used.
	if code, body := s.browser(cookie, http.MethodPost, fmt.Sprintf("%s/%d/publish", s.apps(s.one, space), appID), map[string]any{
		"version": id(a["version"]), "acknowledge": []any{},
	}); code != http.StatusOK {
		s.t.Fatalf("publish app = %d %v", code, body)
	}
	if code, body := s.browser(cookie, http.MethodPost, fmt.Sprintf("%s/%d/actions", s.apps(s.one, space), appID), map[string]any{
		"name": "add_expense", "surfaceId": apps.SurfaceID(appID), "sourceComponentId": "add",
		"context": map[string]any{"description": row, "amount": "3"},
	}); code != http.StatusOK {
		s.t.Fatalf("add row = %d %v", code, body)
	}
	return appID
}

func (s *stack) docs(ws, space int64) string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d/docs", ws, space)
}

// A token reaches what its user can in its workspace, and nothing else: not another user's
// private space, not a shared space its user isn't in, not another workspace, over REST or
// MCP.
func TestTokensReachOnlyTheirUsersSpacesInTheirWorkspace(t *testing.T) {
	s := newStack(t)
	for _, sp := range []int64{s.carol.private, s.secret} {
		if code, body := s.browser(s.carol.cookie, http.MethodPut, s.docs(s.one, sp)+"/hidden.md", map[string]any{"content": "# Hidden\n\nneedle\n"}); code != http.StatusCreated {
			t.Fatalf("carol writes = %d %v", code, body)
		}
	}
	if code, body := s.browser(s.alice.cookie, http.MethodPut, s.docs(s.two, s.alicePrivate2)+"/two.md", map[string]any{"content": "# Two\n\nneedle\n"}); code != http.StatusCreated {
		t.Fatalf("alice writes in two = %d %v", code, body)
	}
	carolApps := map[int64]int64{}
	for _, sp := range []int64{s.carol.private, s.secret} {
		carolApps[sp] = s.app(s.carol.cookie, sp, "needle")
	}
	bob, _ := s.newToken(s.bob.cookie, s.one, "bob's agent")
	alice, _ := s.newToken(s.alice.cookie, s.one, "alice's agent")

	// Their own space works, over both.
	if code, body := s.api(bob, http.MethodPut, s.docs(s.one, s.bob.private)+"/mine.md", map[string]any{"content": "# Mine\n"}); code != http.StatusCreated {
		t.Fatalf("bob's token writes his own space = %d %v", code, body)
	}
	if res := s.toolOK(bob, "read_doc", map[string]any{"space_id": s.bob.private, "path": "mine.md"}); res["content"] != "# Mine\n" {
		t.Fatalf("read_doc = %v", res)
	}

	// Another user's private space, and a shared space bob isn't in.
	for _, sp := range []int64{s.carol.private, s.secret} {
		for _, path := range []string{s.docs(s.one, sp), s.docs(s.one, sp) + "/hidden.md", fmt.Sprintf("/api/workspaces/%d/spaces/%d/boards", s.one, sp)} {
			if code, _ := s.api(bob, http.MethodGet, path, nil); code != http.StatusNotFound {
				t.Errorf("bob's token: GET %s = %d, want 404", path, code)
			}
		}
		if code, _ := s.api(bob, http.MethodPut, s.docs(s.one, sp)+"/new.md", map[string]any{"content": "x"}); code != http.StatusNotFound {
			t.Errorf("bob's token writes space %d = %d, want 404", sp, code)
		}
		for _, call := range []struct {
			name string
			args map[string]any
		}{
			{"list_docs", map[string]any{"space_id": sp}},
			{"read_doc", map[string]any{"space_id": sp, "path": "hidden.md"}},
			{"write_doc", map[string]any{"space_id": sp, "path": "new.md", "content": "x"}},
			{"search_docs", map[string]any{"space_id": sp, "query": "needle"}},
			{"list_boards", map[string]any{"space_id": sp}},
			{"list_apps", map[string]any{"space_id": sp}},
			{"show_app", map[string]any{"space_id": sp, "app_id": carolApps[sp]}},
			{"list_app_rows", map[string]any{"space_id": sp, "app_id": carolApps[sp]}},
			{"update_app", map[string]any{"space_id": sp, "app_id": carolApps[sp], "version": 1, "archived": true}},
		} {
			if code, refused, text := s.tool(bob, call.name, call.args); code != http.StatusOK || !refused || text != "error: not found" {
				t.Errorf("bob's token: %s in space %d = %d %v %q, want the error not found", call.name, sp, code, refused, text)
			}
		}
		// Nor carol's app through bob's own space.
		if _, refused, text := s.tool(bob, "list_app_rows", map[string]any{"space_id": s.bob.private, "app_id": carolApps[sp]}); !refused || text != "error: not found" {
			t.Errorf("bob's token reads carol's app %d through his space: %q", carolApps[sp], text)
		}
	}
	res := s.toolOK(bob, "list_spaces", nil)
	for _, sp := range res["spaces"].([]any) {
		if got := id(sp.(map[string]any)["id"]); got != s.bob.private && got != s.team {
			t.Errorf("bob's token lists space %d", got)
		}
	}
	// list_members says nothing about a space bob can't read, as for the spaces' own routes.
	for _, sp := range []int64{s.carol.private, s.secret} {
		if code, refused, text := s.tool(bob, "list_members", map[string]any{"space_id": sp}); !refused || text != "error: not found" {
			t.Errorf("bob's token lists the members of space %d = %d %v %q", sp, code, refused, text)
		}
	}
	if got := s.toolOK(bob, "list_members", map[string]any{"space_id": s.bob.private})["members"].([]any); len(got) != 1 || id(got[0].(map[string]any)["id"]) != s.bob.id {
		t.Errorf("bob's own space's members = %v", got)
	}
	if hits := s.toolOK(bob, "search_docs", map[string]any{"query": "needle"})["results"].([]any); len(hits) != 0 {
		t.Errorf("bob's token finds %v", hits)
	}

	// Another workspace of the same user: alice is a member of two, her token is for one.
	if code, _ := s.api(alice, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces", s.two), nil); code != http.StatusNotFound {
		t.Errorf("alice's token lists two's spaces = %d, want 404", code)
	}
	for _, path := range []string{s.docs(s.two, s.alicePrivate2) + "/two.md", s.docs(s.one, s.alicePrivate2) + "/two.md"} {
		if code, _ := s.api(alice, http.MethodGet, path, nil); code != http.StatusNotFound {
			t.Errorf("alice's token: GET %s = %d, want 404", path, code)
		}
	}
	if code, refused, text := s.tool(alice, "read_doc", map[string]any{"space_id": s.alicePrivate2, "path": "two.md"}); !refused || text != "error: not found" {
		t.Errorf("alice's token reads her space in two over MCP = %d %v %q", code, refused, text)
	}
	for _, sp := range s.toolOK(alice, "list_spaces", nil)["spaces"].([]any) {
		if id(sp.(map[string]any)["id"]) == s.alicePrivate2 {
			t.Error("alice's token lists her space in two")
		}
	}
	if hits := s.toolOK(alice, "search_docs", map[string]any{"query": "needle"})["results"].([]any); len(hits) != 0 {
		t.Errorf("alice's token finds %v", hits)
	}
}

// Revoking a token, its expiry, and its user leaving the workspace end it from the next
// request, over REST and MCP, and it doesn't come back if they rejoin.
func TestRevokedExpiredAndRemovedTokensFailAtOnce(t *testing.T) {
	s := newStack(t)
	spaces := fmt.Sprintf("/api/workspaces/%d/spaces", s.one)
	works := func(token string) bool {
		t.Helper()
		rest, _ := s.api(token, http.MethodGet, spaces, nil)
		code, refused, _ := s.tool(token, "list_spaces", nil)
		if (rest == http.StatusOK) != (code == http.StatusOK && !refused) {
			t.Fatalf("REST %d and MCP %d disagree", rest, code)
		}
		if rest != http.StatusOK && rest != http.StatusUnauthorized {
			t.Fatalf("a failed token = %d, want 401", rest)
		}
		return rest == http.StatusOK
	}

	revoked, made := s.newToken(s.bob.cookie, s.one, "revoked")
	kept, _ := s.newToken(s.bob.cookie, s.one, "kept")
	if !works(revoked) || !works(kept) {
		t.Fatal("new tokens don't work")
	}
	if code, body := s.browser(s.bob.cookie, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/tokens/%d", s.one, id(made["id"])), nil); code != http.StatusNoContent {
		t.Fatalf("revoke = %d %v", code, body)
	}
	if works(revoked) {
		t.Error("a revoked token works")
	}
	if !works(kept) {
		t.Error("revoking one token ended another")
	}
	// Only its owner revokes a token.
	_, carols := s.newToken(s.carol.cookie, s.one, "carol's")
	if code, _ := s.browser(s.alice.cookie, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/tokens/%d", s.one, id(carols["id"])), nil); code != http.StatusNotFound {
		t.Errorf("alice revokes carol's token = %d, want 404", code)
	}

	// Expiry: the token's time passes.
	expiring, made := s.newToken(s.bob.cookie, s.one, "expiring")
	if !works(expiring) {
		t.Fatal("token doesn't work")
	}
	s.exec(`UPDATE access_tokens SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Second).UTC().Format("2006-01-02T15:04:05.000Z"), id(made["id"]))
	if works(expiring) {
		t.Error("an expired token works")
	}
	if code, body := s.browser(s.bob.cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/tokens", s.one), map[string]any{
		"name": "past", "expires_at": "2020-01-01T00:00:00Z",
	}); code != http.StatusBadRequest {
		t.Errorf("a token expiring in the past = %d %v, want 400", code, body)
	}
	future := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	code, body := s.browser(s.bob.cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/tokens", s.one), map[string]any{
		"name": "tomorrow", "expires_at": future.Format(time.RFC3339),
	})
	if code != http.StatusCreated || body["expires_at"] != future.Format("2006-01-02T15:04:05.000Z") || !works(body["token"].(string)) {
		t.Errorf("a token expiring tomorrow = %d %v", code, body)
	}

	// Leaving the workspace.
	if !works(kept) {
		t.Fatal("token doesn't work")
	}
	if code, body := s.browser(s.alice.cookie, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", s.one, s.bob.id), nil); code != http.StatusNoContent {
		t.Fatalf("remove bob = %d %v", code, body)
	}
	if works(kept) {
		t.Error("a removed member's token works")
	}
	s.exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, s.one, s.bob.id)
	if works(kept) {
		t.Error("a token works again after its user rejoins")
	}
}

// Its user's membership is checked on every request, besides tokens being dropped when
// they leave.
func TestTokensNeedTheirUsersMembership(t *testing.T) {
	s := newStack(t)
	token, _ := s.newToken(s.bob.cookie, s.one, "agent")
	s.exec(`DROP TRIGGER memberships_drop_access_tokens`)
	s.exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, s.one, s.bob.id)
	if code, _ := s.api(token, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces", s.one), nil); code != http.StatusUnauthorized {
		t.Errorf("REST = %d, want 401", code)
	}
	if code, _, _ := s.tool(token, "list_spaces", nil); code != http.StatusUnauthorized {
		t.Errorf("MCP = %d, want 401", code)
	}
}

// The database keeps a token's SHA-256, never the token, and only creating it shows it.
func TestTokensAreStoredHashed(t *testing.T) {
	s := newStack(t)
	token, made := s.newToken(s.bob.cookie, s.one, "agent")
	if !strings.HasPrefix(token, auth.AccessTokenPrefix) || len(token) < 40 {
		t.Fatalf("token %q: want ttp_ and 256 random bits", token)
	}
	var hash, name string
	if err := s.db.QueryRow(`SELECT token_hash, name FROM access_tokens WHERE id = ?`, id(made["id"])).Scan(&hash, &name); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(token))
	if hash != hex.EncodeToString(sum[:]) || name != "agent" {
		t.Fatalf("stored %q, %q; want the token's SHA-256", hash, name)
	}
	var stored int
	if err := s.db.QueryRow(`SELECT count(*) FROM access_tokens WHERE instr(token_hash || name || coalesce(expires_at, '') || coalesce(last_used_at, ''), ?) > 0`,
		strings.TrimPrefix(token, auth.AccessTokenPrefix)).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("the token is in the database (%d, %v)", stored, err)
	}
	// It works, and is never shown again.
	if code, _ := s.api(token, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces", s.one), nil); code != http.StatusOK {
		t.Fatalf("token = %d", code)
	}
	rec := s.request(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/tokens", s.one), nil, sameOrigin(s.bob.cookie))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), token) || strings.Contains(rec.Body.String(), hash) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	list := decode(rec)["tokens"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["last_used_at"] == nil || list[0].(map[string]any)["name"] != "agent" {
		t.Fatalf("list = %v, want the token with its last use", list)
	}
	// Another member doesn't see it.
	if code, body := s.browser(s.carol.cookie, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/tokens", s.one), nil); code != http.StatusOK || len(body["tokens"].([]any)) != 0 {
		t.Fatalf("carol's list = %d %v", code, body)
	}
}

// A token never logs in, sets up, accepts invites, manages members, spaces, providers or
// tokens, or reaches any route that doesn't take one: 403, even with a session cookie
// alongside. A token is alice's, an admin, so only the token can be why.
func TestTokensCannotCreateTokensOrChangeTheAccount(t *testing.T) {
	s := newStack(t)
	token, made := s.newToken(s.alice.cookie, s.one, "agent")
	ws := fmt.Sprintf("/api/workspaces/%d", s.one)
	refused := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, ws + "/tokens", nil},
		{http.MethodPost, ws + "/tokens", map[string]any{"name": "another"}},
		{http.MethodDelete, fmt.Sprintf("%s/tokens/%d", ws, id(made["id"])), nil},
		{http.MethodGet, "/api/setup", nil},
		{http.MethodPost, "/api/setup", map[string]any{"email": "x@example.com", "name": "X", "password": fakePassword, "workspace_name": "X", "setup_token": s.setupToken}},
		{http.MethodPost, "/api/login", map[string]any{"email": "alice@example.com", "password": fakePassword}},
		{http.MethodPost, "/api/logout", nil},
		{http.MethodGet, "/api/me", nil},
		{http.MethodPost, "/api/invites/accept", map[string]any{"token": "x"}},
		{http.MethodGet, ws + "/invites", nil},
		{http.MethodPost, ws + "/invites", map[string]any{"email": "new@example.com"}},
		{http.MethodPatch, fmt.Sprintf("%s/members/%d", ws, s.bob.id), map[string]any{"role": "admin"}},
		{http.MethodDelete, fmt.Sprintf("%s/members/%d", ws, s.bob.id), nil},
		{http.MethodPost, ws + "/spaces", map[string]any{"name": "New"}},
		{http.MethodPatch, fmt.Sprintf("%s/spaces/%d", ws, s.team), map[string]any{"archived": true}},
		{http.MethodPost, fmt.Sprintf("%s/spaces/%d/members", ws, s.secret), map[string]any{"user_id": s.alice.id, "role": "editor"}},
		{http.MethodDelete, fmt.Sprintf("%s/spaces/%d/members/%d", ws, s.team, s.bob.id), nil},
		{http.MethodGet, ws + "/providers", nil},
		{http.MethodPost, ws + "/providers", map[string]any{"name": "x"}},
		{http.MethodGet, "/api/me/providers", nil},
		{http.MethodPut, ws + "/search/settings", map[string]any{}},
		{http.MethodPut, fmt.Sprintf("%s/spaces/%d/search/embedding", ws, s.alice.private), map[string]any{}},
		{http.MethodGet, ws + "/conversations", nil},
		{http.MethodPost, ws + "/conversations", map[string]any{}},
		// The app routes take a session only: over a token, apps are the MCP tools'.
		{http.MethodGet, s.apps(s.one, s.alice.private), nil},
		{http.MethodPost, s.apps(s.one, s.alice.private), map[string]any{"name": "x"}},
		{http.MethodGet, s.apps(s.one, s.alice.private) + "/1/surface", nil},
		{http.MethodPost, s.apps(s.one, s.alice.private) + "/1/actions", map[string]any{"name": "x"}},
	}
	for _, withSession := range []bool{false, true} {
		for _, c := range refused {
			with := []func(*http.Request){bearer(token)}
			if withSession {
				with = append(with, sameOrigin(s.alice.cookie))
			}
			rec := s.request(c.method, c.path, c.body, with...)
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "access tokens can't be used here") {
				t.Errorf("%s %s with a token (and a session: %v) = %d %s, want 403", c.method, c.path, withSession, rec.Code, rec.Body)
			}
			if sessionCookie(rec) != "" {
				t.Errorf("%s %s with a token set a session cookie", c.method, c.path)
			}
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM access_tokens`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d tokens (%v), want just the one", n, err)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", s.one), nil); code != http.StatusOK || len(body["members"].([]any)) != 3 {
		t.Fatalf("members = %d %v", code, body)
	}

	// The routes that take one.
	board := fmt.Sprintf("%s/spaces/%d/boards/%d", ws, s.alice.private, s.alicePersonalBoard)
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{http.MethodGet, ws + "/members", nil, http.StatusOK},
		{http.MethodGet, ws + "/spaces", nil, http.StatusOK},
		{http.MethodGet, fmt.Sprintf("%s/spaces/%d", ws, s.team), nil, http.StatusOK},
		{http.MethodGet, fmt.Sprintf("%s/spaces/%d/members", ws, s.team), nil, http.StatusOK},
		{http.MethodPut, s.docs(s.one, s.alice.private) + "/plan.md", map[string]any{"content": "# Plan\n"}, http.StatusCreated},
		{http.MethodGet, s.docs(s.one, s.alice.private), nil, http.StatusOK},
		{http.MethodGet, fmt.Sprintf("%s/spaces/%d/history", ws, s.alice.private), nil, http.StatusOK},
		{http.MethodGet, ws + "/search?q=plan", nil, http.StatusOK},
		{http.MethodGet, fmt.Sprintf("%s/spaces/%d/search?q=plan", ws, s.alice.private), nil, http.StatusOK},
		{http.MethodPost, board + "/cards", map[string]any{"title": "Ship"}, http.StatusCreated},
		{http.MethodGet, board + "/cards", nil, http.StatusOK},
	} {
		if rec := s.request(c.method, c.path, c.body, bearer(token)); rec.Code != c.want {
			t.Errorf("%s %s with a token = %d %s, want %d", c.method, c.path, rec.Code, rec.Body, c.want)
		}
	}
}

// Changes made with a token are the user's, marked with the token's name, over REST and MCP.
func TestTokenChangesAreTheUsersMarkedWithTheToken(t *testing.T) {
	s := newStack(t)
	token, _ := s.newToken(s.alice.cookie, s.one, "laptop")
	space := s.docs(s.one, s.alice.private)
	if code, body := s.api(token, http.MethodPut, space+"/rest.md", map[string]any{"content": "# REST\n"}); code != http.StatusCreated {
		t.Fatalf("PUT = %d %v", code, body)
	}
	s.toolOK(token, "write_doc", map[string]any{"space_id": s.alice.private, "path": "mcp.md", "content": "# MCP\n"})
	for _, path := range []string{"rest.md", "mcp.md"} {
		code, body := s.browser(s.alice.cookie, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces/%d/history/%s", s.one, s.alice.private, path), nil)
		commit := body["commits"].([]any)[0].(map[string]any)
		if code != http.StatusOK || commit["agent"] != "laptop (access token)" || commit["author"].(map[string]any)["email"] != "alice@example.com" {
			t.Errorf("%s history = %d %v", path, code, commit)
		}
	}

	board := fmt.Sprintf("/api/workspaces/%d/spaces/%d/boards/%d/cards", s.one, s.alice.private, s.alicePersonalBoard)
	code, card := s.api(token, http.MethodPost, board, map[string]any{"title": "REST card"})
	if code != http.StatusCreated {
		t.Fatalf("create card = %d %v", code, card)
	}
	mcpCard := s.toolOK(token, "create_card", map[string]any{"space_id": s.alice.private, "board_id": s.alicePersonalBoard, "title": "MCP card"})
	comment := s.toolOK(token, "comment_card", map[string]any{"space_id": s.alice.private, "board_id": s.alicePersonalBoard, "card_id": id(mcpCard["id"]), "body": "done"})
	want := map[string]any{"id": float64(s.alice.id), "name": "Alice", "agent": true, "via": "laptop"}
	for what, by := range map[string]any{"REST card": card["created_by"], "MCP card": mcpCard["created_by"], "MCP card's update": mcpCard["updated_by"], "comment": comment["author"]} {
		if fmt.Sprint(by) != fmt.Sprint(want) {
			t.Errorf("%s by %v, want %v", what, by, want)
		}
	}
	// Apps too, and their rows are read over MCP as the browser shows them.
	appID := s.app(s.alice.cookie, s.alice.private, "coffee")
	mcpApp := s.toolOK(token, "update_app", map[string]any{"space_id": s.alice.private, "app_id": appID, "version": 2, "name": "Spending"})
	if by := mcpApp["updated_by"]; fmt.Sprint(by) != fmt.Sprint(want) {
		t.Errorf("app's update by %v, want %v", by, want)
	}
	if by := mcpApp["created_by"].(map[string]any); by["agent"] != false || by["via"] != nil {
		t.Errorf("a browser's app by %v", by)
	}
	rows := s.toolOK(token, "list_app_rows", map[string]any{"space_id": s.alice.private, "app_id": appID})["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["description"] != "coffee" {
		t.Errorf("list_app_rows = %v", rows)
	}

	// The browser's own changes stay unmarked.
	code, own := s.browser(s.alice.cookie, http.MethodPost, board, map[string]any{"title": "Mine"})
	if by := own["created_by"].(map[string]any); code != http.StatusCreated || by["agent"] != false || by["via"] != nil {
		t.Errorf("a browser's card by %v", by)
	}
}

// The browser's CSRF protection is what it was: cross-site requests and non-JSON bodies are
// refused with a session. A token doesn't get around it either, on /api or /mcp.
func TestCSRFIsUnchanged(t *testing.T) {
	s := newStack(t)
	token, _ := s.newToken(s.alice.cookie, s.one, "agent")
	path := s.docs(s.one, s.alice.private) + "/x.md"
	crossSite := func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }
	otherOrigin := func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }
	textPlain := func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }
	body := map[string]any{"content": "x"}
	for _, c := range []struct {
		name string
		with []func(*http.Request)
		want int
	}{
		{"session, cross-site", []func(*http.Request){sameOrigin(s.alice.cookie), crossSite}, http.StatusForbidden},
		{"session, other origin", []func(*http.Request){func(r *http.Request) { r.Header.Set("Cookie", "taskto_session="+s.alice.cookie) }, otherOrigin}, http.StatusForbidden},
		{"session, text/plain", []func(*http.Request){sameOrigin(s.alice.cookie), textPlain}, http.StatusUnsupportedMediaType},
		{"token, cross-site", []func(*http.Request){bearer(token), crossSite}, http.StatusForbidden},
		{"token, other origin", []func(*http.Request){bearer(token), otherOrigin}, http.StatusForbidden},
		{"token, text/plain", []func(*http.Request){bearer(token), textPlain}, http.StatusUnsupportedMediaType},
		{"session, same origin", []func(*http.Request){sameOrigin(s.alice.cookie)}, http.StatusCreated},
	} {
		if rec := s.request(http.MethodPut, path, body, c.with...); rec.Code != c.want {
			t.Errorf("%s = %d %s, want %d", c.name, rec.Code, rec.Body, c.want)
		}
	}

	msg := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for _, c := range []struct {
		name string
		with func(*http.Request)
		want int
	}{
		{"cross-site", crossSite, http.StatusForbidden},
		{"other origin", otherOrigin, http.StatusForbidden},
		{"text/plain", textPlain, http.StatusUnsupportedMediaType},
		{"a session alone", func(r *http.Request) {
			r.Header.Del("Authorization")
			r.Header.Set("Cookie", "taskto_session="+s.alice.cookie)
		}, http.StatusUnauthorized},
		{"plain", func(*http.Request) {}, http.StatusOK},
	} {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(msg))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("Authorization", "Bearer "+token)
		c.with(r)
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, r)
		if rec.Code != c.want {
			t.Errorf("/mcp, %s = %d %s, want %d", c.name, rec.Code, rec.Body, c.want)
		}
	}
}

// bearerTransport adds an access token to every request, as an MCP client's config does.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// An MCP client (the official SDK's) connects, lists the tools, calls them, and is cut
// off when its token is revoked.
func TestMCPClient(t *testing.T) {
	s := newStack(t)
	token, made := s.newToken(s.alice.cookie, s.one, "client")
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: s.mcpURL, HTTPClient: &http.Client{Transport: bearerTransport{token}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Every tool but the conversation agent's own (the browser's, whose hand-over to the user
	// happens in a conversation).
	defs := map[string]agenttools.Definition{}
	for _, d := range agenttools.Definitions() {
		if !d.ConversationOnly {
			defs[d.Name] = d
		}
	}
	if len(list.Tools) != len(defs) {
		t.Fatalf("%d tools, want %d", len(list.Tools), len(defs))
	}
	for _, tool := range list.Tools {
		d, ok := defs[tool.Name]
		if !ok || tool.Description != d.Description || tool.Annotations.ReadOnlyHint != d.ReadOnly {
			t.Errorf("tool %s isn't as defined", tool.Name)
		}
	}

	for _, tool := range list.Tools {
		if strings.HasPrefix(tool.Name, "browser_") {
			t.Errorf("MCP lists %s", tool.Name)
		}
	}
	if res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "browser_open", Arguments: map[string]any{"url": "https://example.com"}}); err == nil && !res.IsError {
		t.Fatalf("browser_open over MCP = %v", res)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "write_doc", Arguments: map[string]any{
		"space_id": s.alice.private, "path": "from-mcp.md", "content": "# From MCP\n",
	}})
	if err != nil || res.IsError {
		t.Fatalf("write_doc = %v, %v", res, err)
	}
	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "read_doc", Arguments: map[string]any{"space_id": s.alice.private, "path": "from-mcp.md"}})
	if err != nil || res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "# From MCP") {
		t.Fatalf("read_doc = %v, %v", res, err)
	}
	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "read_doc", Arguments: map[string]any{"space_id": s.alice.private, "path": "from-mcp.md", "extra": 1}})
	if err != nil || !res.IsError {
		t.Fatalf("an unknown argument = %v, %v; want a tool error", res, err)
	}

	if code, _ := s.browser(s.alice.cookie, http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/tokens/%d", s.one, id(made["id"])), nil); code != http.StatusNoContent {
		t.Fatalf("revoke = %d", code)
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_spaces", Arguments: map[string]any{}}); err == nil {
		t.Fatal("a call with a revoked token succeeded")
	}
}

// /mcp without a token says how to get one, and never echoes what it was given.
func TestMCPWithoutAToken(t *testing.T) {
	s := newStack(t)
	for _, token := range []string{"", auth.AccessTokenPrefix + "not-a-real-token-but-long-enough-xxxxxxxxxxxx", "a-session-token"} {
		code, _, text := s.tool(token, "list_spaces", nil)
		if code != http.StatusUnauthorized || (token != "" && strings.Contains(text, token)) {
			t.Errorf("token %q = %d %s, want 401", token, code, text)
		}
	}
}
