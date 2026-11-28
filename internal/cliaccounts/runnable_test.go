//go:build unix

package cliaccounts

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/providers"
)

var everyModel = []providers.ModelChoice{{ID: "default", Name: "Default"}, {ID: "opus", Name: "Opus"}, {ID: "sonnet", Name: "Sonnet"}}

func (e *turnEnv) offered(c *client, conv int64, cli cliworker.CLI) []string {
	e.t.Helper()
	p := conversations.Principal{Conversation: conv, AgentConversation: conv, Workspace: e.ws, User: c.id}
	got, err := e.svc.RunnableModels(context.Background(), p, cli, everyModel)
	if err != nil {
		e.t.Fatal(err)
	}
	out := []string{}
	for _, m := range got {
		out = append(out, m.ID)
	}
	return out
}

func (e *turnEnv) runnable(c *client, cli cliworker.CLI) bool {
	e.t.Helper()
	return len(e.offered(c, e.conversation(c), cli)) > 0
}

func (e *turnEnv) setMode(c *client, mode any) {
	e.t.Helper()
	expect(e.t, e.owner.do(http.MethodPatch, sharedPath(e.ws)+"/modes", map[string]any{"members": map[string]any{itoa(c.id): mode}}), http.StatusOK)
}

func (e *turnEnv) connectedOwn(c *client, state string) {
	e.t.Helper()
	if _, err := e.db.Exec(`DELETE FROM cli_accounts WHERE user_id = ?`, c.id); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO cli_accounts (user_id, cli, slot, state, auth_method) VALUES (?, 'claude', 1, ?, 'claude.ai')`, c.id, state); err != nil {
		e.t.Fatal(err)
	}
}

func TestRunnableOwnAccountOnlyNeedsAConnectedAccount(t *testing.T) {
	e := newTurnEnv(t)
	e.setMode(e.other, "own")
	if e.runnable(e.other, cliworker.Claude) {
		t.Fatal("a member in own-account mode with no account can run Claude")
	}
	e.connectedOwn(e.other, "reconnect")
	if e.runnable(e.other, cliworker.Claude) {
		t.Fatal("an account that needs a new sign-in can run Claude")
	}
	e.connectedOwn(e.other, "connected")
	if !e.runnable(e.other, cliworker.Claude) {
		t.Fatal("a connected own account can't run Claude")
	}
	if e.runnable(e.other, cliworker.Codex) {
		t.Fatal("a Claude account runs Codex")
	}
}

func TestRunnableSharedOnlyNeedsASharedAccountOfThatCLI(t *testing.T) {
	e := newTurnEnv(t)
	e.setMode(e.other, "shared")
	e.connectedOwn(e.other, "connected")
	if e.runnable(e.other, cliworker.Claude) {
		t.Fatal("a member in shared-only mode with no shared account can run Claude on their own account")
	}
	e.owner.addShared(e.ws, cliworker.Codex, newProvider(t).URL)
	if e.runnable(e.other, cliworker.Claude) {
		t.Fatal("a shared Codex account lets Claude run")
	}
	if !e.runnable(e.other, cliworker.Codex) {
		t.Fatal("a shared Codex account can't run Codex")
	}
}

func TestRunnableFollowsAModeChange(t *testing.T) {
	e := newTurnEnv(t)
	e.connectedOwn(e.other, "connected")
	e.owner.addShared(e.ws, cliworker.Claude, newProvider(t).URL)
	e.setMode(e.other, "own")
	if !e.runnable(e.other, cliworker.Claude) {
		t.Fatal("own mode with a connected account can't run Claude")
	}
	e.setMode(e.other, "shared")
	if !e.runnable(e.other, cliworker.Claude) {
		t.Fatal("shared mode with a shared account can't run Claude")
	}
	if _, err := e.db.Exec(`DELETE FROM cli_shared_accounts`); err != nil {
		t.Fatal(err)
	}
	if e.runnable(e.other, cliworker.Claude) {
		t.Fatal("shared mode after the shared account was removed can run Claude")
	}
	other := e.setup2()
	if e.runnable(other, cliworker.Claude) {
		t.Fatal("a member with no account can run Claude")
	}
}

func (e *turnEnv) setup2() *client { return e.invite(e.owner, e.ws, "third@example.com") }

func TestRunnableModelsOfASharedAccountAreOnlyTheModelItIsPinnedTo(t *testing.T) {
	e := newTurnEnv(t)
	ids := func(c *client) []string { return e.offered(c, e.conversation(c), cliworker.Claude) }
	e.owner.addShared(e.ws, cliworker.Claude, newProvider(t).URL)
	e.setMode(e.other, "shared")
	if got := ids(e.other); len(got) != 3 {
		t.Fatalf("a shared account with no pinned model offers %v, want every model", got)
	}
	if _, err := e.db.Exec(`UPDATE cli_shared_accounts SET model = 'sonnet'`); err != nil {
		t.Fatal(err)
	}
	if got := ids(e.other); len(got) != 1 || got[0] != "sonnet" {
		t.Fatalf("a shared account pinned to sonnet offers %v, want only sonnet, the model its turns run on", got)
	}
	if _, err := e.db.Exec(`UPDATE cli_shared_accounts SET model = 'haiku'`); err != nil {
		t.Fatal(err)
	}
	if got := ids(e.other); len(got) != 0 {
		t.Fatalf("a shared account pinned to a model the list lacks offers %v, want none", got)
	}
	e.setMode(e.other, "own")
	e.connectedOwn(e.other, "connected")
	if got := ids(e.other); len(got) != 3 {
		t.Fatalf("a connected own account offers %v, want every model", got)
	}
}

func (e *turnEnv) pinnedAccounts() int64 {
	e.t.Helper()
	p := newProvider(e.t)
	opus := e.owner.addShared(e.ws, cliworker.Claude, p.URL)
	sonnet := e.owner.addShared(e.ws, cliworker.Claude, p.URL)
	for id, model := range map[int64]string{opus.ID: "opus", sonnet.ID: "sonnet"} {
		if _, err := e.db.Exec(`UPDATE cli_shared_accounts SET model = ? WHERE id = ?`, model, id); err != nil {
			e.t.Fatal(err)
		}
	}
	e.other.setSwitching(map[string]any{"enabled": true, "queue": []map[string]any{{"shared": opus.ID}, {"shared": sonnet.ID}}})
	conv := e.conversation(e.other)
	if d := done(e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "opus", Prompt: "hello"})); d.Status != cliworker.StatusOK {
		e.t.Fatalf("the first turn = %+v", d)
	}
	e.other.setSwitching(map[string]any{"queue": []map[string]any{{"shared": sonnet.ID}, {"shared": opus.ID}}})
	return conv
}

func TestRunnableModelsFollowTheAccountTheConversationKeeps(t *testing.T) {
	e := newTurnEnv(t)
	kept := e.pinnedAccounts()
	if got := e.offered(e.other, e.conversation(e.other), cliworker.Claude); len(got) != 1 || got[0] != "sonnet" {
		t.Fatalf("a new conversation is offered %v, want only sonnet, the first account of the queue", got)
	}
	if got := e.offered(e.other, kept, cliworker.Claude); len(got) != 1 || got[0] != "opus" {
		t.Fatalf("a conversation that last ran on the opus account is offered %v, want only opus: its turns keep that account whatever the queue says", got)
	}
}

func TestAPinnedTurnRunsOnlyOnTheModelItWasContinuedOn(t *testing.T) {
	e := newTurnEnv(t)
	conv := e.pinnedAccounts()
	d := done(e.turn(conv, turnBody{Turn: "t2", CLI: "claude", Model: "sonnet", Prompt: "again", Pinned: true}))
	if d.Status != cliworker.StatusFailed || !strings.Contains(d.Text, "continued on") {
		t.Fatalf("a pinned turn on a model its account doesn't run = %+v, want a refusal", d)
	}
	if d := done(e.turn(conv, turnBody{Turn: "t3", CLI: "claude", Model: "opus", Prompt: "again", Pinned: true})); d.Status != cliworker.StatusOK {
		t.Fatalf("a pinned turn on the model its account runs = %+v", d)
	}
}

func TestAPinnedTurnIsNotMovedToAnotherAccountByALimit(t *testing.T) {
	e, conv, models := switchEnv(t, true)
	events := e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it", Pinned: true})
	if d := done(events); d.Status != cliworker.StatusFailed {
		t.Fatalf("done = %+v, want the limited account's failure", d)
	}
	for _, ev := range events {
		if ev.Type == cliworker.EventNotice {
			t.Fatalf("a pinned turn switched: %+v", ev)
		}
	}
	dirs, err := e.worker.AccountDirs(e.acct(e.owner.id), cliworker.Codex, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(clitest.Calls(t, dirs.Config(cliworker.Codex))); n != 0 || len(models.list()) != 0 {
		t.Fatalf("a pinned turn ran %d turns on Codex and wrote the model %v", n, models.list())
	}
}
