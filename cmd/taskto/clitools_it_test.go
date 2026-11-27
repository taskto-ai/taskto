//go:build unix

package main

// taskto's tools in CLI conversations (docs/cli-tools.md), end to end: the server binary with
// its real sidecar, and the fake Claude Code, which reads the MCP settings the worker wrote and
// calls the tools at the endpoint on the proxy's listener with its turn's token. The owner's
// turn lists their spaces and writes a document once the owner allows it; the member's turn
// reaches nothing of the owner's; the token of a turn that ended is refused. Gated by
// TASKTO_AGENT_IT=1 like the other integration tests.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

func TestCLIToolsThroughTheServer(t *testing.T) {
	if os.Getenv("TASKTO_AGENT_IT") != "1" {
		t.Skip("set TASKTO_AGENT_IT=1 to run the agent sidecar integration tests")
	}
	entry, err := filepath.Abs("../../agent/dist/index.js")
	if err != nil {
		t.Fatal(err)
	}
	dir := shortDataDir(t)
	bin := filepath.Join(dir, "taskto")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	fakes := filepath.Join(dir, "fakes")
	if err := os.Mkdir(fakes, 0o700); err != nil {
		t.Fatal(err)
	}
	clitest.Install(t, fakes)
	t.Setenv("PATH", fakes+string(os.PathListSeparator)+os.Getenv("PATH"))
	decodeInto := func(raw []byte, v any) {
		t.Helper()
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatalf("decoding %s: %v", raw, err)
		}
	}
	dataDir := filepath.Join(dir, "data")
	// local-unsafe: two users on one OS user, as no real install may run; the isolation checked
	// here is the server's.
	server, addr := startServer(t, bin, "-addr", "127.0.0.1:0", "-data-dir", dataDir, "-agent", "-agent-entry", entry,
		"-insecure-cookies", "-cli-workers", "local-unsafe")
	layout := cliworker.Layout{Root: filepath.Join(dataDir, "cli")}

	newAPI := func() *api {
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		return &api{t: t, base: "http://" + addr, http: &http.Client{Jar: jar}}
	}
	var me struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
		Workspaces []struct {
			ID int64 `json:"id"`
		} `json:"workspaces"`
	}
	owner := newAPI()
	token := setupTokenLine.FindStringSubmatch(server.output.String())
	if token == nil {
		t.Fatal("the server logged no setup token")
	}
	decodeInto(owner.expect(http.StatusCreated, http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": "fake-password-for-tests", "workspace_name": "Home",
		"setup_token": token[1],
	}), &me)
	ownerID, ws := me.User.ID, me.Workspaces[0].ID
	var invite struct {
		Token string `json:"token"`
	}
	decodeInto(owner.expect(http.StatusCreated, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", ws),
		map[string]string{"email": "member@example.com"}), &invite)
	member := newAPI()
	decodeInto(member.expect(http.StatusCreated, http.MethodPost, "/api/invites/accept", map[string]string{
		"token": invite.Token, "name": "Member", "password": "fake-password-for-tests",
	}), &me)
	memberID := me.User.ID

	privateOf := func(a *api) int64 {
		var list struct {
			Spaces []struct {
				ID   int64  `json:"id"`
				Kind string `json:"kind"`
			} `json:"spaces"`
		}
		decodeInto(a.expect(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces", ws), nil), &list)
		for _, s := range list.Spaces {
			if s.Kind == "private" {
				return s.ID
			}
		}
		t.Fatal("no private space")
		return 0
	}
	ownerPrivate, memberPrivate := privateOf(owner), privateOf(member)
	member.expect(http.StatusCreated, http.MethodPut, fmt.Sprintf("/api/workspaces/%d/spaces/%d/docs/secret.md", ws, memberPrivate),
		map[string]string{"content": "# The member's\n"})

	connect := func(a *api, user int64) {
		var info cliworker.LoginInfo
		decodeInto(a.expect(http.StatusOK, http.MethodPost, "/api/me/cli-accounts/claude/login", map[string]any{}), &info)
		a.expect(http.StatusAccepted, http.MethodPost, "/api/me/cli-accounts/claude/login/code", map[string]string{"code": clitest.GoodCode})
		waitFor(t, "claude to connect", func() bool {
			var list struct {
				Accounts []struct {
					CLI   string `json:"cli"`
					State string `json:"state"`
				} `json:"accounts"`
			}
			decodeInto(a.expect(http.StatusOK, http.MethodGet, "/api/me/cli-accounts", nil), &list)
			for _, acct := range list.Accounts {
				if acct.CLI == "claude" && acct.State == "connected" {
					return true
				}
			}
			return false
		})
	}
	connect(owner, ownerID)
	connect(member, memberID)

	base := fmt.Sprintf("/api/workspaces/%d/conversations", ws)
	newConversation := func(a *api) int64 {
		var conv struct {
			ID int64 `json:"id"`
		}
		decodeInto(a.expect(http.StatusCreated, http.MethodPost, base, map[string]any{
			"title": "tools", "model": map[string]string{"provider": "cli-claude", "model_id": "default"},
		}), &conv)
		return conv.ID
	}
	answers := func(a *api, conv int64) []string {
		var msgs struct {
			Messages []struct {
				Role string `json:"role"`
				Text string `json:"text"`
			} `json:"messages"`
		}
		status, body := a.do(http.MethodGet, fmt.Sprintf("%s/%d/messages", base, conv), nil)
		if status == http.StatusServiceUnavailable {
			return nil
		}
		if status != http.StatusOK {
			t.Fatalf("messages = %d %s", status, body)
		}
		decodeInto(body, &msgs)
		var out []string
		for _, m := range msgs.Messages {
			if m.Role == "assistant" && m.Text != "" {
				out = append(out, m.Text)
			}
		}
		return out
	}
	result := func(answer, tool string) string {
		for _, line := range strings.Split(answer, "\n") {
			if rest, ok := strings.CutPrefix(line, "mcp "+tool+": "); ok {
				return rest
			}
		}
		return "(no call)"
	}

	// The owner's turn: a read that doesn't ask, then a write that does, and runs once allowed.
	ownerClaude := homeOf(t, layout, ownerID).Config(cliworker.Claude)
	clitest.WriteScenario(t, ownerClaude, clitest.Scenario{MCP: []clitest.MCPCall{
		{Tool: "list_spaces"},
		{Tool: "write_doc", Args: map[string]any{"space_id": ownerPrivate, "path": "from-claude.md", "content": "# Written by the CLI\n"}},
	}})
	conv := newConversation(owner)
	convBase := fmt.Sprintf("%s/%d", base, conv)
	owner.expect(http.StatusAccepted, http.MethodPost, convBase+"/messages", map[string]string{"content": "note it down"})
	var questions struct {
		Requests []cliworker.PermissionRequest `json:"requests"`
	}
	waitLong(t, "the write's question", func() bool {
		decodeInto(owner.expect(http.StatusOK, http.MethodGet, convBase+"/cli-permissions", nil), &questions)
		return len(questions.Requests) == 1
	})
	q := questions.Requests[0]
	if q.Tool != "mcp__taskto__write_doc" || !strings.Contains(q.Input, "from-claude.md") {
		t.Fatalf("question = %+v", q)
	}
	member.expect(http.StatusNotFound, http.MethodPost, convBase+"/cli-permissions/"+q.ID, map[string]bool{"allow": true})
	owner.expect(http.StatusNoContent, http.MethodPost, convBase+"/cli-permissions/"+q.ID, map[string]bool{"allow": true})
	var got []string
	waitLong(t, "the owner's answer", func() bool {
		got = answers(owner, conv)
		return len(got) > 0
	})
	answer := got[len(got)-1]
	spaces := result(answer, "list_spaces")
	if !strings.HasPrefix(spaces, "ok ") || !strings.Contains(spaces, fmt.Sprintf(`"id":%d,`, ownerPrivate)) || strings.Contains(spaces, fmt.Sprintf(`"id":%d,`, memberPrivate)) {
		t.Fatalf("list_spaces = %s", spaces)
	}
	if w := result(answer, "write_doc"); !strings.HasPrefix(w, "ok ") {
		t.Fatalf("write_doc = %s", w)
	}
	// The document is the owner's, committed by the agent.
	var history struct {
		Commits []struct {
			Author struct{ Name string } `json:"author"`
			Agent  string                `json:"agent"`
		} `json:"commits"`
	}
	decodeInto(owner.expect(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces/%d/history/from-claude.md", ws, ownerPrivate), nil), &history)
	if len(history.Commits) != 1 || history.Commits[0].Author.Name != "Owner" || history.Commits[0].Agent != "taskto.ai agent" {
		t.Fatalf("history = %+v", history)
	}

	// The turn is over: its token is refused at the endpoint it was given.
	calls := clitest.Calls(t, ownerClaude)
	if len(calls) == 0 {
		t.Fatal("the owner's CLI never ran")
	}
	last := calls[len(calls)-1]
	var endpoint, ended string
	for i, a := range last.Args {
		if a == "--mcp-config" && i+1 < len(last.Args) {
			var settings struct {
				Servers map[string]struct {
					URL string `json:"url"`
				} `json:"mcpServers"`
			}
			b, err := os.ReadFile(last.Args[i+1])
			if err != nil {
				t.Fatal(err)
			}
			decodeInto(b, &settings)
			endpoint = settings.Servers["taskto"].URL
		}
	}
	for _, v := range last.Env {
		if token, ok := strings.CutPrefix(v, "TASKTO_TOOLS_TOKEN="); ok {
			ended = token
		}
	}
	if !strings.HasSuffix(endpoint, "/cli-mcp") || ended == "" {
		t.Fatalf("endpoint %q, token found %v", endpoint, ended != "")
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_spaces","arguments":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+ended)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the ended turn's token = %d %s", res.StatusCode, body)
	}

	// The member's turn reaches nothing of the owner's: not their private space, not their
	// document, whatever the model asks for.
	clitest.WriteScenario(t, homeOf(t, layout, memberID).Config(cliworker.Claude), clitest.Scenario{MCP: []clitest.MCPCall{
		{Tool: "list_spaces"},
		{Tool: "read_doc", Args: map[string]any{"space_id": ownerPrivate, "path": "from-claude.md"}},
		{Tool: "read_doc", Args: map[string]any{"space_id": memberPrivate, "path": "secret.md"}},
	}})
	theirs := newConversation(member)
	member.expect(http.StatusAccepted, http.MethodPost, fmt.Sprintf("%s/%d/messages", base, theirs), map[string]string{"content": "look around"})
	waitLong(t, "the member's answer", func() bool {
		got = answers(member, theirs)
		return len(got) > 0
	})
	answer = got[len(got)-1]
	spaces = result(answer, "list_spaces")
	if !strings.Contains(spaces, fmt.Sprintf(`"id":%d,`, memberPrivate)) || strings.Contains(spaces, fmt.Sprintf(`"id":%d,`, ownerPrivate)) {
		t.Fatalf("the member's list_spaces = %s", spaces)
	}
	var reads []string
	for _, line := range strings.Split(answer, "\n") {
		if rest, ok := strings.CutPrefix(line, "mcp read_doc: "); ok {
			reads = append(reads, rest)
		}
	}
	if len(reads) != 2 || reads[0] != "refused error: not found" || !strings.HasPrefix(reads[1], "ok ") || !strings.Contains(reads[1], "The member's") {
		t.Fatalf("the member's reads = %q", reads)
	}
	// A CLI's request to take over the browser is kept with its turn, for the conversation's
	// card. The browser is off here, so the call itself is refused: the card shows what the
	// model asked of the user, as for the built-in agent's call.
	const plea = "Sign in to the shop, then press Done."
	clitest.WriteScenario(t, ownerClaude, clitest.Scenario{MCP: []clitest.MCPCall{
		{Tool: "browser_help", Args: map[string]any{"tab": "T1", "reason": "login", "message": plea}},
	}})
	helpConv := newConversation(owner)
	helpBase := fmt.Sprintf("%s/%d", base, helpConv)
	owner.expect(http.StatusAccepted, http.MethodPost, helpBase+"/messages", map[string]string{"content": "get my orders"})
	waitLong(t, "browser_help's question", func() bool {
		decodeInto(owner.expect(http.StatusOK, http.MethodGet, helpBase+"/cli-permissions", nil), &questions)
		return len(questions.Requests) == 1
	})
	owner.expect(http.StatusNoContent, http.MethodPost, helpBase+"/cli-permissions/"+questions.Requests[0].ID, map[string]bool{"allow": true})
	waitLong(t, "the answer after browser_help", func() bool { return len(answers(owner, helpConv)) > 0 })
	if _, body := owner.do(http.MethodGet, helpBase+"/entries", nil); !strings.Contains(string(body), `"help":{`) || !strings.Contains(string(body), plea) {
		t.Fatalf("the turn didn't keep the request: %s", body)
	}

	// Each user's CLI got its own token, never the other's.
	memberCalls := clitest.Calls(t, homeOf(t, layout, memberID).Config(cliworker.Claude))
	if len(memberCalls) == 0 {
		t.Fatal("the member's CLI never ran")
	}
	if env := strings.Join(memberCalls[len(memberCalls)-1].Env, "\n"); strings.Contains(env, ended) || !strings.Contains(env, "TASKTO_TOOLS_TOKEN=ttm1.") {
		t.Fatal("the member's CLI didn't get a token of its own")
	}
}

// homeOf is the directories of user's account in the CLI worker at layout: the home its records
// give the account of that number (the worker keeps an account under its identity).
func homeOf(t *testing.T, layout cliworker.Layout, user int64) cliworker.Dirs {
	t.Helper()
	d, ok := homeIfAny(t, layout, user)
	if !ok {
		t.Fatalf("the CLI worker has no account of user %d", user)
	}
	return d
}

// homeIfAny is homeOf, false when the worker never gave user's account a home (nothing of the
// account's ever ran on it).
func homeIfAny(t *testing.T, layout cliworker.Layout, user int64) (cliworker.Dirs, bool) {
	t.Helper()
	dir := filepath.Join(layout.Root, ".worker", "accounts")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return cliworker.Dirs{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // G304: the test's own data directory
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Home    int64 `json:"home"`
			User    int64 `json:"user"`
			Retired bool  `json:"retired"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		if r.User == user && !r.Retired {
			return layout.Home(r.Home), true
		}
	}
	return cliworker.Dirs{}, false
}
