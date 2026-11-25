package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/agenttools"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/db"
)

// The tokens below are obviously fake: the server only looks at the prefix and the hash.
const (
	tokenA       = auth.AccessTokenPrefix + "fake-token-of-alice-in-one"
	tokenB       = auth.AccessTokenPrefix + "fake-token-of-bob-in-two"
	tokenExpired = auth.AccessTokenPrefix + "fake-token-expired"
)

// fixture is two workspaces with a member and a private space each, a token for each member,
// and the endpoint's handlers over them.
type fixture struct {
	db             *sql.DB
	tools          *agenttools.Service
	wsA, wsB       int64
	userA, userB   int64
	spaceA, spaceB int64
	endpoint       http.Handler
	browserTools   []string // the conversation-only tools
	noTokenTools   []string // the tools of conversations that the access tokens' endpoint leaves out
	definitions    map[string]agenttools.Definition
}

func newFixture(t *testing.T) *fixture {
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
	f := &fixture{db: sqlDB, definitions: map[string]agenttools.Definition{}}
	scalar := func(q string, args ...any) int64 {
		t.Helper()
		var v int64
		if err := sqlDB.QueryRow(q, args...).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	f.wsA = scalar(`INSERT INTO workspaces (name) VALUES ('One') RETURNING id`)
	f.wsB = scalar(`INSERT INTO workspaces (name) VALUES ('Two') RETURNING id`)
	f.userA = scalar(`INSERT INTO users (email, name, password_hash) VALUES ('alice@example.com', 'Alice', 'x') RETURNING id`)
	f.userB = scalar(`INSERT INTO users (email, name, password_hash) VALUES ('bob@example.com', 'Bob', 'x') RETURNING id`)
	for _, m := range []struct{ ws, user int64 }{{f.wsA, f.userA}, {f.wsB, f.userB}} {
		if _, err := sqlDB.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, m.ws, m.user); err != nil {
			t.Fatal(err)
		}
	}
	f.spaceA = scalar(`SELECT id FROM spaces WHERE workspace_id = ? AND owner_id = ?`, f.wsA, f.userA) // made with the membership
	f.spaceB = scalar(`SELECT id FROM spaces WHERE workspace_id = ? AND owner_id = ?`, f.wsB, f.userB)
	for _, tk := range []struct {
		raw       string
		ws, user  int64
		name      string
		expiresAt any
	}{
		{tokenA, f.wsA, f.userA, "alice's agent", nil},
		{tokenB, f.wsB, f.userB, "bob's agent", nil},
		{tokenExpired, f.wsA, f.userA, "old", "2000-01-01T00:00:00.000Z"},
	} {
		sum := sha256.Sum256([]byte(tk.raw))
		if _, err := sqlDB.Exec(`INSERT INTO access_tokens (user_id, workspace_id, name, token_hash, expires_at) VALUES (?, ?, ?, ?, ?)`,
			tk.user, tk.ws, tk.name, hex.EncodeToString(sum[:]), tk.expiresAt); err != nil {
			t.Fatal(err)
		}
	}

	spaces := access.New(sqlDB, nil)
	f.tools = agenttools.New(agenttools.Options{DB: sqlDB, Access: spaces, Boards: boards.New(sqlDB, spaces), Apps: apps.New(sqlDB, spaces)})
	accounts := auth.New(sqlDB, auth.Options{HashParams: auth.Params{Memory: 64, Time: 1, Threads: 1}})
	f.endpoint = New("test", accounts, f.tools)
	for _, d := range agenttools.Definitions() {
		f.definitions[d.Name] = d
		if d.ConversationOnly {
			f.browserTools = append(f.browserTools, d.Name)
		}
		if d.NoToken {
			f.noTokenTools = append(f.noTokenTools, d.Name)
		}
	}
	if len(f.browserTools) == 0 || len(f.noTokenTools) == 0 {
		t.Fatal("tools.json has no conversation-only or no-token tools: the listing tests would prove nothing")
	}
	return f
}

// rpc posts one JSON-RPC message to h, as a stateless MCP client, with token if it isn't empty.
func rpc(t *testing.T, h http.Handler, token, method string, params any) *httptest.ResponseRecorder {
	t.Helper()
	msg, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(msg))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-06-18")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

type listedTool struct {
	Name        string `json:"name"`
	Annotations struct {
		ReadOnlyHint  bool  `json:"readOnlyHint"`
		OpenWorldHint *bool `json:"openWorldHint"`
	} `json:"annotations"`
}

// listTools asks h for its tools with token.
func listTools(t *testing.T, h http.Handler, token string) []listedTool {
	t.Helper()
	rec := rpc(t, h, token, "tools/list", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list = %d %s", rec.Code, rec.Body)
	}
	var reply struct {
		Result struct {
			Tools []listedTool `json:"tools"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || reply.Error != nil {
		t.Fatalf("tools/list reply %s (%v)", rec.Body, err)
	}
	return reply.Result.Tools
}

func names(tools []listedTool) []string {
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, tl.Name)
	}
	slices.Sort(out)
	return out
}

// callTool runs a tool and returns the HTTP status, whether the tool refused, and its text.
func callTool(t *testing.T, h http.Handler, token, name string, args map[string]any) (int, bool, string) {
	t.Helper()
	rec := rpc(t, h, token, "tools/call", map[string]any{"name": name, "arguments": args})
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
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("%s: reply %s (%v)", name, rec.Body, err)
	}
	if reply.Error != nil {
		return rec.Code, true, "rpc error: " + reply.Error.Message
	}
	if len(reply.Result.Content) != 1 {
		t.Fatalf("%s: reply %s", name, rec.Body)
	}
	return rec.Code, reply.Result.IsError, reply.Result.Content[0].Text
}

// forTurns is the turns' endpoint, for the caller authorize answers with.
func (f *fixture) forTurns(browser bool, authorize Authorize) http.Handler {
	return ForTurns("test", authorize, f.tools, browser)
}

func (f *fixture) callerA(*http.Request) (agenttools.Caller, error) {
	return agenttools.Caller{Workspace: f.wsA, User: f.userA, Conversation: 1}, nil
}

// /mcp lists the tools of people's own agents: every tool but the conversation-only ones and the
// ones a token doesn't get, with their descriptions' hints, whatever token asks.
func TestMCPListsNoConversationOnlyTools(t *testing.T) {
	f := newFixture(t)
	var want []string
	for name, d := range f.definitions {
		if !d.ConversationOnly && !d.NoToken {
			want = append(want, name)
		}
	}
	slices.Sort(want)
	for _, token := range []string{tokenA, tokenB} {
		got := names(listTools(t, f.endpoint, token))
		if !slices.Equal(got, want) {
			t.Errorf("/mcp lists %v, want %v", got, want)
		}
		for _, name := range slices.Concat(f.browserTools, f.noTokenTools) {
			if slices.Contains(got, name) {
				t.Errorf("/mcp lists the conversation-only tool %s", name)
			}
		}
	}
	// And one can't be called by name either: it isn't there.
	for _, name := range slices.Concat(f.browserTools, f.noTokenTools) {
		if _, refused, text := callTool(t, f.endpoint, tokenA, name, map[string]any{}); !refused || !strings.Contains(text, "unknown tool") {
			t.Errorf("/mcp calls %s: refused %v, %q; want an unknown tool", name, refused, text)
		}
	}
}

// The turns' endpoint has the tools a token doesn't get, and the browser tools too while the agent
// browser is on; with it off, it is /mcp's list and those.
func TestForTurnsListsBrowserToolsOnlyWhenOn(t *testing.T) {
	f := newFixture(t)
	mcpTools := names(listTools(t, f.endpoint, tokenA))

	wantOff := slices.Concat(mcpTools, f.noTokenTools)
	slices.Sort(wantOff)
	off := names(listTools(t, f.forTurns(false, f.callerA), "any-turn-token"))
	if !slices.Equal(off, wantOff) {
		t.Errorf("turns' tools with the browser off = %v, want /mcp's and the conversations' %v", off, wantOff)
	}
	on := names(listTools(t, f.forTurns(true, f.callerA), "any-turn-token"))
	wantOn := slices.Concat(wantOff, f.browserTools)
	slices.Sort(wantOn)
	if !slices.Equal(on, wantOn) {
		t.Errorf("turns' tools with the browser on = %v, want %v", on, wantOn)
	}
}

// Every tool says whether it is read-only as tools.json does, so a client may call the reading
// ones without asking, and none says it reaches the open world.
func TestToolsAreAnnotatedAsTheirDefinitionsSay(t *testing.T) {
	f := newFixture(t)
	for _, h := range []struct {
		name string
		h    http.Handler
		tok  string
	}{
		{"/mcp", f.endpoint, tokenA},
		{"turns", f.forTurns(true, f.callerA), "any-turn-token"},
	} {
		listed := listTools(t, h.h, h.tok)
		if len(listed) == 0 {
			t.Fatalf("%s lists nothing", h.name)
		}
		for _, tl := range listed {
			d := f.definitions[tl.Name]
			if tl.Annotations.ReadOnlyHint != d.ReadOnly {
				t.Errorf("%s: %s readOnlyHint = %v, tools.json says read_only %v", h.name, tl.Name, tl.Annotations.ReadOnlyHint, d.ReadOnly)
			}
			if tl.Annotations.OpenWorldHint == nil || *tl.Annotations.OpenWorldHint {
				t.Errorf("%s: %s openWorldHint = %v, want false", h.name, tl.Name, tl.Annotations.OpenWorldHint)
			}
		}
	}
	// A few that must be right whatever the file says: what only reads, and what changes things.
	byName := map[string]listedTool{}
	for _, tl := range listTools(t, f.endpoint, tokenA) {
		byName[tl.Name] = tl
	}
	for _, name := range []string{"list_spaces", "read_doc", "list_cards", "list_apps", "show_app", "list_app_rows", "query_app_rows", "app_catalog"} {
		if tl, ok := byName[name]; !ok || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s should be listed as read-only: %+v", name, tl)
		}
	}
	for _, name := range []string{"write_doc", "create_card", "update_card", "move_card", "comment_card", "set_blocked", "create_app", "update_app"} {
		if tl, ok := byName[name]; !ok || tl.Annotations.ReadOnlyHint {
			t.Errorf("%s changes things and must not be listed as read-only: %+v", name, tl)
		}
	}
}

// A token is one user's in one workspace: it sees that workspace's spaces and no other's, and
// a space of another workspace is as unknown as one that doesn't exist.
func TestATokenOfOneWorkspaceCannotActInAnother(t *testing.T) {
	f := newFixture(t)

	for _, c := range []struct {
		token        string
		own, foreign int64
		name, other  string
	}{
		{tokenA, f.spaceA, f.spaceB, "alice", "bob"},
		{tokenB, f.spaceB, f.spaceA, "bob", "alice"},
	} {
		code, refused, text := callTool(t, f.endpoint, c.token, "list_spaces", nil)
		var listed struct {
			Spaces []struct {
				ID int64 `json:"id"`
			} `json:"spaces"`
		}
		if code != http.StatusOK || refused || json.Unmarshal([]byte(text), &listed) != nil {
			t.Fatalf("%s's list_spaces = %d %v %q", c.name, code, refused, text)
		}
		if len(listed.Spaces) != 1 || listed.Spaces[0].ID != c.own {
			t.Errorf("%s's token lists %+v, want only space %d", c.name, listed.Spaces, c.own)
		}
		for _, call := range []struct {
			tool string
			args map[string]any
		}{
			{"list_apps", map[string]any{"space_id": c.foreign}},
			{"list_boards", map[string]any{"space_id": c.foreign}},
			{"list_members", map[string]any{"space_id": c.foreign}},
			{"create_app", map[string]any{"space_id": c.foreign, "name": "x", "definition": map[string]any{}}},
		} {
			code, refused, text := callTool(t, f.endpoint, c.token, call.tool, call.args)
			if code != http.StatusOK || !refused || text != "error: not found" {
				t.Errorf("%s's token: %s in %s's space = %d %v %q, want the error not found", c.name, call.tool, c.other, code, refused, text)
			}
		}
		// Their own space answers.
		if code, refused, text := callTool(t, f.endpoint, c.token, "list_apps", map[string]any{"space_id": c.own}); code != http.StatusOK || refused {
			t.Errorf("%s's token: list_apps in their own space = %d %v %q", c.name, code, refused, text)
		}
	}
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM apps`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d apps (%v), want none made across workspaces", n, err)
	}

	// The turns' endpoint is the same for the workspace its caller is in.
	turns := f.forTurns(false, f.callerA)
	if code, refused, text := callTool(t, turns, "turn", "list_apps", map[string]any{"space_id": f.spaceB}); code != http.StatusOK || !refused || text != "error: not found" {
		t.Errorf("a turn of workspace one in workspace two's space = %d %v %q, want not found", code, refused, text)
	}
	if code, refused, text := callTool(t, turns, "turn", "list_apps", map[string]any{"space_id": f.spaceA}); code != http.StatusOK || refused {
		t.Errorf("a turn in its own space = %d %v %q", code, refused, text)
	}
}

// Without a valid token nothing is served: 401 with a challenge, whatever the message is, and
// the expired, unknown, and revoked look alike.
func TestMCPRefusesWithoutAValidToken(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db.Exec(`DELETE FROM memberships WHERE user_id = ?`, f.userB); err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{
		"none":                   "",
		"unknown":                auth.AccessTokenPrefix + "fake-token-nobody-has",
		"not a token":            "fake-bearer-without-prefix",
		"expired":                tokenExpired,
		"its user left the team": tokenB,
	} {
		rec := rpc(t, f.endpoint, token, "tools/list", map[string]any{})
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: tools/list = %d %s (challenge %q), want 401 with a challenge", name, rec.Code, rec.Body, rec.Header().Get("WWW-Authenticate"))
		}
		if token != "" && strings.Contains(rec.Body.String(), strings.TrimPrefix(token, auth.AccessTokenPrefix)) {
			t.Errorf("%s: the reply echoes the token: %s", name, rec.Body)
		}
	}
	// A session cookie is no credential here.
	r := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(`{}`))
	r.Header.Set("Cookie", "taskto_session=anything")
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.endpoint.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a cookie = %d, want 401", rec.Code)
	}
}

// A browser's cross-origin request is refused before the token is looked at.
func TestMCPRefusesCrossOriginRequests(t *testing.T) {
	f := newFixture(t)
	for name, h := range map[string]http.Handler{"/mcp": f.endpoint, "turns": f.forTurns(false, f.callerA)} {
		r := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+tokenA)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: a cross-site request = %d %s, want 403", name, rec.Code, rec.Body)
		}
	}
}

// ForTurns answers what authorize says: a refusal with its status (and a challenge for a 401),
// any other error as a plain 500 that says nothing of it.
func TestForTurnsAnswersAuthorizeRefusals(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		name      string
		err       error
		want      int
		challenge bool
	}{
		{"unauthorized", &agenttools.Refusal{Status: http.StatusUnauthorized, Message: "no valid turn"}, http.StatusUnauthorized, true},
		{"forbidden", &agenttools.Refusal{Status: http.StatusForbidden, Message: "tools are off"}, http.StatusForbidden, false},
		{"database", errors.New("disk on fire at /var/lib/secret"), http.StatusInternalServerError, false},
	} {
		h := f.forTurns(false, func(*http.Request) (agenttools.Caller, error) { return agenttools.Caller{}, c.err })
		rec := rpc(t, h, "turn", "tools/list", map[string]any{})
		if rec.Code != c.want || (rec.Header().Get("WWW-Authenticate") != "") != c.challenge {
			t.Errorf("%s: %d %s (challenge %q), want %d", c.name, rec.Code, rec.Body, rec.Header().Get("WWW-Authenticate"), c.want)
		}
		if strings.Contains(rec.Body.String(), "disk on fire") || strings.Contains(rec.Body.String(), "/var/lib") {
			t.Errorf("%s: the reply says what failed: %s", c.name, rec.Body)
		}
	}
}
