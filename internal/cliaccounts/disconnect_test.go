//go:build unix

package cliaccounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

// Disconnecting an account through the API stops the turn that is using its login and waits for
// it before the login's files go; another user's turn goes on, and so does the owner's turn on
// another account.
func TestDisconnectStopsTheTurnUsingTheLogin(t *testing.T) {
	e := newTurnEnv(t)
	conv := e.conversation(e.owner)
	start := func(id string, user int64, slot cliworker.Slot, s clitest.Scenario) *cliworker.Turn {
		t.Helper()
		dirs, err := e.dirsSlot(user, cliworker.Claude, slot), error(nil)
		if err != nil {
			t.Fatal(err)
		}
		config := dirs.ConfigOf(cliworker.Claude, slot)
		clitest.SignIn(t, "claude", config)
		clitest.WriteScenario(t, config, s)
		turn, _, err := e.worker.Start(cliworker.TurnSpec{ID: id, User: user, Identity: e.acct(user).Identity, CLI: cliworker.Claude, Slot: slot, Conversation: conv,
			Session: fmt.Sprintf("00000000-0000-4000-8000-%012d", len(id)*100+int(user)), Prompt: "work"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { e.worker.Stop(id) })
		eventually(t, "the turn didn't start its tool", func() bool {
			events, _, _ := turn.Events(0)
			for _, ev := range events {
				if ev.Type == cliworker.EventTool {
					return true
				}
			}
			return false
		})
		return turn
	}
	mine := start("c1-mine", e.owner.id, "", clitest.Scenario{SleepMS: 60_000, TermWriteMS: 300})
	second := start("c1-second", e.owner.id, "2", clitest.Scenario{SleepMS: 60_000})
	others := start("c1-others", e.other.id, "", clitest.Scenario{SleepMS: 60_000})
	if _, err := e.db.Exec(`INSERT INTO cli_accounts (user_id, cli, slot, state, auth_method) VALUES (?, 'claude', 1, 'connected', 'claude.ai')`, e.owner.id); err != nil {
		t.Fatal(err)
	}

	expect(t, e.owner.do(http.MethodDelete, base+"/claude", nil), http.StatusNoContent)
	if !mine.Over() {
		t.Fatal("the account was disconnected while a turn still ran on its login")
	}
	config := e.dirs(e.owner.id).Config(cliworker.Claude)
	if _, err := os.Stat(filepath.Join(config, clitest.LateWrite)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stopped turn wrote into the disconnected account's directory: %v", err)
	}
	if second.Over() || others.Over() {
		t.Errorf("another turn was stopped: the owner's second account over %v, the other user's over %v", second.Over(), others.Over())
	}
}

// The worker's refusals while an account is being removed are answers the web app translates.
func TestRemovalRefusalsAreAPIErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   apierr.Code
	}{
		{cliworker.ErrRemoving, http.StatusConflict, apierr.CodeCLIAccountRemoving},
		{cliworker.ErrBusy, http.StatusConflict, apierr.CodeCLIAccountBusy},
		{cliworker.ErrForgotten, http.StatusGone, apierr.CodeCLIAccountForgotten},
	} {
		rec := httptest.NewRecorder()
		workerError(rec, httptest.NewRequest(http.MethodPost, "/", nil), ref{cli: cliworker.Claude}, fmt.Errorf("on the worker: %w", tc.err))
		var body struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&body)
		if rec.Code != tc.status || body.Code != string(tc.code) {
			t.Errorf("%v: answer = %d %q, want %d %q", tc.err, rec.Code, body.Code, tc.status, tc.code)
		}
	}
}
