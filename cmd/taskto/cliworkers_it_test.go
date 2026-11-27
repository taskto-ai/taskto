//go:build unix

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

func TestCLIWorkersThroughTheServer(t *testing.T) {
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

	dataDir := filepath.Join(dir, "data")
	server, addr := startServer(t, bin, "-addr", "127.0.0.1:0", "-data-dir", dataDir, "-agent", "-agent-entry", entry,
		"-insecure-cookies", "-cli-workers", "local-unsafe")
	layout := cliworker.Layout{Root: filepath.Join(dataDir, "cli")}
	_, socketDir, _ := agentPaths(dataDir)
	sidecar := agentrt.New(filepath.Join(socketDir, "agent.sock"))

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
	if err := json.Unmarshal(owner.expect(http.StatusCreated, http.MethodPost, "/api/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": "fake-password-for-tests", "workspace_name": "Home",
		"setup_token": token[1],
	}), &me); err != nil {
		t.Fatal(err)
	}
	ownerID, ws := me.User.ID, me.Workspaces[0].ID
	var invite struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(owner.expect(http.StatusCreated, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", ws),
		map[string]string{"email": "member@example.com"}), &invite); err != nil {
		t.Fatal(err)
	}
	member := newAPI()
	if err := json.Unmarshal(member.expect(http.StatusCreated, http.MethodPost, "/api/invites/accept", map[string]string{
		"token": invite.Token, "name": "Member", "password": "fake-password-for-tests",
	}), &me); err != nil {
		t.Fatal(err)
	}
	memberID := me.User.ID

	state := func(a *api, cli string) string {
		var list struct {
			Accounts []struct {
				CLI   string `json:"cli"`
				State string `json:"state"`
			} `json:"accounts"`
		}
		if err := json.Unmarshal(a.expect(http.StatusOK, http.MethodGet, "/api/me/cli-accounts", nil), &list); err != nil {
			t.Fatal(err)
		}
		for _, acct := range list.Accounts {
			if acct.CLI == cli {
				return acct.State
			}
		}
		return ""
	}
	connect := func(a *api, user int64, cli string) {
		var info cliworker.LoginInfo
		if err := json.Unmarshal(a.expect(http.StatusOK, http.MethodPost, "/api/me/cli-accounts/"+cli+"/login", map[string]any{}), &info); err != nil {
			t.Fatal(err)
		}
		if info.State != cliworker.LoginWaiting || !info.URLTrusted {
			t.Fatalf("login = %+v", info)
		}
		if cli == "claude" {
			a.expect(http.StatusAccepted, http.MethodPost, "/api/me/cli-accounts/claude/login/code", map[string]string{"code": clitest.GoodCode})
		} else {
			clitest.Approve(t, layout.User(user).Config(cliworker.CLI(cli)))
		}
		waitFor(t, cli+" to connect", func() bool { return state(a, cli) == "connected" })
	}

	connect(owner, ownerID, "claude")
	connect(owner, ownerID, "codex")

	var models struct {
		Providers []struct {
			Provider string `json:"provider"`
			Scope    string `json:"scope"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(owner.expect(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/models", ws), nil), &models); err != nil {
		t.Fatal(err)
	}
	offered := map[string]string{}
	for _, p := range models.Providers {
		offered[p.Provider] = p.Scope
	}
	if offered["cli-claude"] != "cli" || offered["cli-codex"] != "cli" || offered["cli-grok"] != "cli" {
		t.Fatalf("models = %+v", models)
	}

	base := fmt.Sprintf("/api/workspaces/%d/conversations", ws)
	newConversation := func(a *api, provider string) int64 {
		var conv struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(a.expect(http.StatusCreated, http.MethodPost, base, map[string]any{
			"title": provider, "model": map[string]string{"provider": provider, "model_id": "default"},
		}), &conv); err != nil {
			t.Fatal(err)
		}
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
		if err := json.Unmarshal(body, &msgs); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range msgs.Messages {
			if m.Role == "assistant" && m.Text != "" {
				out = append(out, m.Text)
			}
		}
		return out
	}
	ask := func(a *api, conv int64, content string) string {
		before := len(answers(a, conv))
		a.expect(http.StatusAccepted, http.MethodPost, fmt.Sprintf("%s/%d/messages", base, conv), map[string]string{"content": content})
		var got []string
		waitLong(t, "an answer to "+content, func() bool {
			got = answers(a, conv)
			return len(got) > before
		})
		return got[len(got)-1]
	}
	claudeCalls := func(user int64) int { return len(clitest.Calls(t, layout.User(user).Config(cliworker.Claude))) }

	conv := newConversation(owner, "cli-claude")
	if got := ask(owner, conv, "hello from the test"); got != "echo: hello from the test" {
		t.Fatalf("answer = %q", got)
	}
	if got := ask(owner, conv, "and again"); got != "echo: and again (turn 2 of this session)" {
		t.Fatalf("second answer = %q", got)
	}

	codexConv := newConversation(owner, "cli-codex")
	if got := ask(owner, codexConv, "hello codex"); got != "echo: hello codex" {
		t.Fatalf("codex answer = %q", got)
	}

	ownerCodex := layout.User(ownerID).Config(cliworker.Codex)
	clitest.WriteScenario(t, ownerCodex, clitest.Scenario{Tool: &clitest.ToolUse{Name: "Bash", Input: map[string]any{"command": "touch proof.txt"}}})
	codexBase := fmt.Sprintf("%s/%d", base, codexConv)
	answered := len(answers(owner, codexConv))
	owner.expect(http.StatusAccepted, http.MethodPost, codexBase+"/messages", map[string]string{"content": "make the file"})
	var questions struct {
		Requests []cliworker.PermissionRequest `json:"requests"`
	}
	waitLong(t, "the CLI's question", func() bool {
		if err := json.Unmarshal(owner.expect(http.StatusOK, http.MethodGet, codexBase+"/cli-permissions", nil), &questions); err != nil {
			t.Fatal(err)
		}
		return len(questions.Requests) == 1
	})
	if q := questions.Requests[0]; q.Tool != "Bash" || q.Input != "touch proof.txt" {
		t.Fatalf("question = %+v", q)
	}
	member.expect(http.StatusNotFound, http.MethodGet, codexBase+"/cli-permissions", nil)
	member.expect(http.StatusNotFound, http.MethodPost, codexBase+"/cli-permissions/"+questions.Requests[0].ID, map[string]bool{"allow": true})
	owner.expect(http.StatusNoContent, http.MethodPost, codexBase+"/cli-permissions/"+questions.Requests[0].ID, map[string]bool{"allow": true})
	var after []string
	waitLong(t, "the answer after the question", func() bool {
		after = answers(owner, codexConv)
		return len(after) > answered
	})
	if last := after[len(after)-1]; !strings.Contains(last, "tool Bash: ran") {
		t.Fatalf("answer after allowing = %q", last)
	}
	if _, body := owner.do(http.MethodGet, codexBase+"/entries", nil); !strings.Contains(string(body), "? Codex asks to use Bash: touch proof.txt") || !strings.Contains(string(body), "✓ allowed") {
		t.Errorf("the conversation doesn't show the question and its answer: %s", body)
	}
	owner.expect(http.StatusOK, http.MethodPatch, codexBase, map[string]string{"cli_permissions": "allow_all"})
	if got := ask(owner, codexConv, "make it again"); !strings.Contains(got, "tool Bash: ran") {
		t.Fatalf("answer with allow_all = %q", got)
	}
	clitest.WriteScenario(t, ownerCodex, clitest.Scenario{})

	before := claudeCalls(ownerID)
	theirs := newConversation(member, "cli-claude")
	if got := ask(member, theirs, "use the owner's account"); !strings.Contains(got, "isn't connected") {
		t.Fatalf("the member's answer = %q", got)
	}
	if claudeCalls(ownerID) != before || claudeCalls(memberID) != 0 {
		t.Fatalf("calls: owner %d (was %d), member %d", claudeCalls(ownerID), before, claudeCalls(memberID))
	}

	ownerClaude := layout.User(ownerID).Config(cliworker.Claude)
	clitest.WriteScenario(t, ownerClaude, clitest.Scenario{Expired: true})
	n := len(answers(owner, conv))
	owner.expect(http.StatusAccepted, http.MethodPost, fmt.Sprintf("%s/%d/messages", base, conv), map[string]string{"content": "after the expiry"})
	waitLong(t, "the account to need a Reconnect", func() bool { return state(owner, "claude") == "reconnect" })
	if got := answers(owner, conv); len(got) != n {
		t.Fatalf("the turn answered instead of waiting: %q", got[len(got)-1])
	}
	clitest.WriteScenario(t, ownerClaude, clitest.Scenario{})
	connect(owner, ownerID, "claude")
	var got []string
	waitLong(t, "the conversation to go on after the Reconnect", func() bool {
		got = answers(owner, conv)
		return len(got) > n
	})
	if last := got[len(got)-1]; !strings.HasPrefix(last, "echo: after the expiry") {
		t.Fatalf("answer after the Reconnect = %q", last)
	}

	clitest.WriteScenario(t, ownerClaude, clitest.Scenario{SleepMS: 4000})
	before = claudeCalls(ownerID)
	n = len(answers(owner, conv))
	owner.expect(http.StatusAccepted, http.MethodPost, fmt.Sprintf("%s/%d/messages", base, conv), map[string]string{"content": "slow work"})
	waitFor(t, "the CLI to start", func() bool { return claudeCalls(ownerID) == before+1 })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	health, err := sidecar.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(health.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitLong(t, "the answer after the sidecar's restart", func() bool {
		got = answers(owner, conv)
		return len(got) > n
	})
	if last := got[len(got)-1]; !strings.HasPrefix(last, "echo: slow work") {
		t.Fatalf("answer after the restart = %q", last)
	}
	if calls := claudeCalls(ownerID); calls != before+1 {
		t.Fatalf("the CLI ran %d times for one turn", calls-before)
	}

	owner.expect(http.StatusOK, http.MethodPut, "/api/me/cli-switching", map[string]any{
		"enabled": true, "queue": []map[string]string{{"account": "claude"}, {"account": "codex"}},
	})
	clitest.WriteScenario(t, ownerClaude, clitest.Scenario{Fail: "You've hit your usage limit · resets 3pm"})
	if got := ask(owner, conv, "switch me"); !strings.HasPrefix(got, "echo: ") || !strings.Contains(got, "switch me") {
		t.Fatalf("answer after the switch = %q", got)
	}
	var model struct {
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(owner.expect(http.StatusOK, http.MethodGet, fmt.Sprintf("%s/%d/model", base, conv), nil), &model); err != nil {
		t.Fatal(err)
	}
	if model.Provider != "cli-codex" {
		t.Fatalf("model after the switch = %+v", model)
	}
	if got := ask(owner, conv, "and on"); !strings.HasPrefix(got, "echo: and on") {
		t.Fatalf("answer on Codex = %q", got)
	}
}

func waitLong(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
