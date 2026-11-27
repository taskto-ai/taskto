//go:build unix

package cliaccounts

import (
	"context"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/providers"
)

func (e *turnEnv) runnable(c *client, cli cliworker.CLI) bool {
	e.t.Helper()
	ok, _, err := e.svc.Runnable(context.Background(), e.ws, c.id, cli)
	if err != nil {
		e.t.Fatal(err)
	}
	return ok
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
	models := []providers.ModelChoice{{ID: "default", Name: "Default"}, {ID: "opus", Name: "Opus"}, {ID: "sonnet", Name: "Sonnet"}}
	ids := func(c *client) []string {
		t.Helper()
		got, err := e.svc.RunnableModels(context.Background(), e.ws, c.id, cliworker.Claude, models)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, m := range got {
			out = append(out, m.ID)
		}
		return out
	}
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
