//go:build unix

package cliaccounts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/agenttools"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/mcpserver"
	"taskto.ai/taskto/internal/serverkey"
)

func newToolsEnv(t *testing.T) *turnEnv {
	t.Helper()
	e := newTurnEnv(t)
	key, err := serverkey.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spaces := access.New(e.db, nil)
	tools := agenttools.New(agenttools.Options{
		DB: e.db, Key: key, Access: spaces, Boards: boards.New(e.db, spaces), Apps: apps.New(e.db, spaces),
		Knowledge: knowledge.New(filepath.Join(t.TempDir(), "data"), knowledge.Options{}),
	})
	var readOnly []string
	for _, d := range agenttools.Definitions() {
		if d.ReadOnly && !d.ConversationOnly {
			readOnly = append(readOnly, d.Name)
		}
	}
	e.svc.SetTools(Tools{Handler: mcpserver.ForTurns("test", e.svc.ToolsCaller, tools, false), ReadOnly: readOnly})
	return e
}

func (e *turnEnv) privateSpace(c *client) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(`SELECT id FROM spaces WHERE workspace_id = ? AND kind = 'private' AND owner_id = ?`, e.ws, c.id).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func mcpResult(answer, tool string) string {
	for _, line := range strings.Split(answer, "\n") {
		if rest, ok := strings.CutPrefix(line, "mcp "+tool+": "); ok {
			return rest
		}
	}
	return "(no call)"
}

func (e *turnEnv) toolsToken(c *client, cli cliworker.CLI) string {
	e.t.Helper()
	calls := clitest.Calls(e.t, e.layout.User(c.id).Config(cli))
	if len(calls) == 0 {
		e.t.Fatal("the CLI never ran")
	}
	for _, v := range calls[len(calls)-1].Env {
		if token, ok := strings.CutPrefix(v, "TASKTO_TOOLS_TOKEN="); ok {
			return token
		}
	}
	e.t.Fatal("the CLI got no tools token")
	return ""
}

func (e *turnEnv) listSpaces(token string) (int, string) {
	e.t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_spaces","arguments":{}}}`
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodPost, e.proxy.URL+ToolsPath, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestTurnsUseTaskToTools(t *testing.T) {
	e := newToolsEnv(t)
	ownerSpace, otherSpace := e.privateSpace(e.owner), e.privateSpace(e.other)
	for _, c := range []*client{e.owner, e.other} {
		c.connect(cliworker.Claude)
	}
	for _, c := range []struct {
		who         *client
		mine, other int64
	}{{e.owner, ownerSpace, otherSpace}, {e.other, otherSpace, ownerSpace}} {
		clitest.WriteScenario(t, e.layout.User(c.who.id).Config(cliworker.Claude), clitest.Scenario{MCP: []clitest.MCPCall{
			{Tool: "list_spaces"},
			{Tool: "write_doc", Args: map[string]any{"space_id": c.mine, "path": "from-the-cli.md", "content": "# Mine\n"}},
			{Tool: "read_doc", Args: map[string]any{"space_id": c.other, "path": "from-the-cli.md"}},
			{Tool: "write_doc", Args: map[string]any{"space_id": c.other, "path": "planted.md", "content": "# Not yours\n"}},
		}})
		conv := e.conversation(c.who)
		if _, err := e.db.Exec(`UPDATE conversations SET cli_permissions = 'allow_all' WHERE id = ?`, conv); err != nil {
			t.Fatal(err)
		}
		answer := done(e.turn(conv, turnBody{Turn: "tools", CLI: "claude", Model: "default", Prompt: "use the tools"})).Text

		spaces := mcpResult(answer, "list_spaces")
		if !strings.HasPrefix(spaces, "ok ") || !strings.Contains(spaces, fmt.Sprintf(`"id":%d,`, c.mine)) || strings.Contains(spaces, fmt.Sprintf(`"id":%d,`, c.other)) {
			t.Errorf("user %d: list_spaces = %s", c.who.id, spaces)
		}
		if got := mcpResult(answer, "write_doc"); !strings.HasPrefix(got, "ok ") || !strings.Contains(got, `"commit"`) {
			t.Errorf("user %d: write_doc in their own space = %s", c.who.id, got)
		}
		if got := mcpResult(answer, "read_doc"); got != "refused error: not found" {
			t.Errorf("user %d: read_doc in the other's space = %s", c.who.id, got)
		}
		lines := strings.Split(answer, "\n")
		var writes []string
		for _, l := range lines {
			if strings.HasPrefix(l, "mcp write_doc: ") {
				writes = append(writes, l)
			}
		}
		if len(writes) != 2 || writes[1] != "mcp write_doc: refused error: not found" {
			t.Errorf("user %d: writing into the other's space = %q", c.who.id, writes)
		}

		token := e.toolsToken(c.who, cliworker.Claude)
		if status, body := e.listSpaces(token); status != http.StatusUnauthorized {
			t.Errorf("user %d: the ended turn's token = %d %s", c.who.id, status, body)
		}
	}
}

func TestToolsTokenRefusals(t *testing.T) {
	e := newToolsEnv(t)
	conv := e.conversation(e.owner)
	ctx := t.Context()
	dirs, err := e.layout.Ensure(e.owner.id)
	if err != nil {
		t.Fatal(err)
	}
	clitest.SignIn(t, "claude", dirs.Config(cliworker.Claude))
	clitest.WriteScenario(t, dirs.Config(cliworker.Claude), clitest.Scenario{SleepMS: 20000})
	id := fmt.Sprintf("c%d-running", conv)
	turn, _, err := e.worker.Start(cliworker.TurnSpec{ID: id, User: e.owner.id, CLI: cliworker.Claude, Conversation: conv,
		Session: "00000000-0000-4000-8000-000000000042", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.worker.Stop(id) })

	caller := func(token string) (agenttools.Caller, int) {
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, ToolsPath, bytes.NewReader(nil))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		c, err := e.svc.ToolsCaller(r)
		var refused *agenttools.Refusal
		if errors.As(err, &refused) {
			return c, refused.Status
		}
		if err != nil {
			t.Fatal(err)
		}
		return c, http.StatusOK
	}
	mint := func(claims toolsClaims) string {
		token, err := e.svc.mintToolsToken(claims)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	claims := toolsClaims{User: e.owner.id, Conversation: conv, Turn: id}

	good := mint(claims)
	if c, status := caller(good); status != http.StatusOK || c != (agenttools.Caller{Workspace: e.ws, User: e.owner.id, Conversation: conv}) {
		t.Fatalf("a running turn's token: %d %+v", status, c)
	}

	forged := good[:len(good)-4] + "AAAA"
	proxyToken, err := e.svc.mintToken(proxyClaims{Account: 1, User: e.owner.id, Conversation: conv, Turn: id})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(toolsClaims{Nonce: "old", User: e.owner.id, Conversation: conv, Turn: id, Expires: time.Now().Add(-time.Minute).Unix()})
	expired := toolsTokenPrefix + b64.EncodeToString(payload) + "." + b64.EncodeToString(e.svc.shared.Key.MAC(toolsTokenPurpose, payload))
	otherKey, err := serverkey.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload, _ = json.Marshal(toolsClaims{Nonce: "x", User: e.owner.id, Conversation: conv, Turn: id, Expires: time.Now().Add(time.Hour).Unix()})
	otherServer := toolsTokenPrefix + b64.EncodeToString(payload) + "." + b64.EncodeToString(otherKey.MAC(toolsTokenPurpose, payload))
	for name, token := range map[string]string{
		"forged": forged, "a proxy token": proxyToken, "expired": expired, "another server's": otherServer,
		"none": "", "malformed": "ttm1.nope", "an access token's shape": "ttp_0123456789abcdef0123456789abcdef",
	} {
		if _, status := caller(token); status != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, status)
		}
	}

	for name, c := range map[string]toolsClaims{
		"a turn the worker doesn't know":     {User: e.owner.id, Conversation: conv, Turn: "c1-unknown"},
		"the turn, for another user":         {User: e.other.id, Conversation: conv, Turn: id},
		"the turn, for another conversation": {User: e.owner.id, Conversation: e.conversation(e.owner), Turn: id},
	} {
		if _, status := caller(mint(c)); status == http.StatusOK {
			t.Errorf("%s: accepted", name)
		}
	}

	replayed := mint(claims)
	if _, status := caller(good); status != http.StatusUnauthorized {
		t.Errorf("the token a replay replaced: %d", status)
	}
	if _, err := e.db.Exec(`UPDATE conversations SET archived_at = CURRENT_TIMESTAMP WHERE id = ?`, conv); err != nil {
		t.Fatal(err)
	}
	if _, status := caller(replayed); status != http.StatusForbidden {
		t.Errorf("an archived conversation: %d", status)
	}
	if _, err := e.db.Exec(`UPDATE conversations SET archived_at = NULL WHERE id = ?`, conv); err != nil {
		t.Fatal(err)
	}
	if _, status := caller(replayed); status != http.StatusOK {
		t.Fatalf("back: %d", status)
	}
	if _, err := e.db.Exec(`UPDATE users SET disabled_at = '2026-01-02T03:04:05.000Z' WHERE id = ?`, e.owner.id); err != nil {
		t.Fatal(err)
	}
	if _, status := caller(replayed); status != http.StatusForbidden {
		t.Errorf("its owner disabled: %d", status)
	}
	if _, err := e.db.Exec(`UPDATE users SET disabled_at = NULL WHERE id = ?`, e.owner.id); err != nil {
		t.Fatal(err)
	}
	if _, status := caller(replayed); status != http.StatusOK {
		t.Fatalf("its owner enabled again: %d", status)
	}
	if _, err := e.db.Exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, e.ws, e.owner.id); err != nil {
		t.Fatal(err)
	}
	if _, status := caller(replayed); status != http.StatusForbidden {
		t.Errorf("its owner gone from the workspace: %d", status)
	}
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, e.ws, e.owner.id); err != nil {
		t.Fatal(err)
	}
	if _, status := caller(replayed); status != http.StatusOK {
		t.Fatalf("back in the workspace: %d", status)
	}

	e.worker.Stop(id)
	waitOver(t, turn)
	if _, status := caller(replayed); status != http.StatusUnauthorized {
		t.Errorf("after the turn: %d", status)
	}
	e.svc.mu.Lock()
	clear(e.svc.ended)
	e.svc.mu.Unlock()
	if _, status := caller(replayed); status != http.StatusUnauthorized {
		t.Errorf("after the turn, with the server's list forgotten: %d", status)
	}
}

func waitOver(t *testing.T, turn *cliworker.Turn) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !turn.Over() {
		if time.Now().After(deadline) {
			t.Fatal("the turn didn't end")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTurnsWithoutTaskToTools(t *testing.T) {
	e := newTurnEnv(t)
	e.owner.connect(cliworker.Claude)
	clitest.WriteScenario(t, e.layout.User(e.owner.id).Config(cliworker.Claude), clitest.Scenario{MCP: []clitest.MCPCall{{Tool: "list_spaces"}}})
	answer := done(e.turn(e.conversation(e.owner), turnBody{Turn: "bare", CLI: "claude", Model: "default", Prompt: "go"})).Text
	if got := mcpResult(answer, "list_spaces"); got != "error no MCP settings" {
		t.Errorf("a turn without tools: %s", got)
	}
	if status, _ := e.listSpaces("ttm1.anything.at-all"); status != http.StatusNotFound {
		t.Errorf("the endpoint while the tools are off: %d", status)
	}
}
