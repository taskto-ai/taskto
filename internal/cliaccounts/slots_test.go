//go:build unix

package cliaccounts

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

func TestParseHandle(t *testing.T) {
	for in, want := range map[string]ref{
		"claude":   {cli: cliworker.Claude},
		"codex-2":  {cli: cliworker.Codex, slot: "2"},
		"grok-9":   {cli: cliworker.Grok, slot: "9"},
		"claude-1": {}, "claude-10": {}, "claude-s3": {}, "claude-": {}, "foo": {}, "foo-2": {}, "": {}, "claude-2-2": {},
	} {
		got, ok := parseHandle(in)
		if ok != (want != ref{}) || got != want {
			t.Errorf("parseHandle(%q) = %+v %v", in, got, ok)
		}
		if ok && got.handle() != in {
			t.Errorf("handle of %q = %q", in, got.handle())
		}
	}
}

// A user connects a second account to a CLI next to the first, names it, and disconnects it
// without touching the first.
func TestSeveralAccountsPerCLI(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	owner, ws := e.setup()
	other := e.invite(owner, ws, "other@example.com")

	owner.connect(cliworker.Claude)
	owner.connectAccount("claude-2")
	ids := []string{}
	for _, a := range owner.list().Accounts {
		ids = append(ids, a.ID)
	}
	if strings.Join(ids, " ") != "claude claude-2 codex grok" {
		t.Fatalf("accounts = %v", ids)
	}
	second := owner.accountByID("claude-2")
	if second.Slot != 2 || second.CLI != cliworker.Claude || second.State != Connected {
		t.Fatalf("second account = %+v", second)
	}

	// A label is the account's own.
	rec := owner.do(http.MethodPatch, base+"/claude-2", map[string]string{"label": "  Work  "})
	expect(t, rec, http.StatusOK)
	if a := decode[Account](t, rec); a.Label != "Work" || a.ID != "claude-2" {
		t.Fatalf("patched = %+v", a)
	}
	if a := owner.account(cliworker.Claude); a.Label != "" {
		t.Errorf("the first account got the label: %+v", a)
	}
	expect(t, owner.do(http.MethodPatch, base+"/claude-2", map[string]string{"label": strings.Repeat("x", 41)}), http.StatusBadRequest)
	expect(t, owner.do(http.MethodPatch, base+"/claude-2", map[string]string{"label": "two\nlines"}), http.StatusBadRequest)
	expect(t, owner.do(http.MethodPatch, base+"/claude-3", map[string]string{"label": "none"}), http.StatusNotFound)
	expect(t, owner.do(http.MethodPatch, base+"/claude-2", map[string]any{}), http.StatusBadRequest)

	// Another user's requests name their own accounts: they never reach the owner's.
	expect(t, other.do(http.MethodPatch, base+"/claude-2", map[string]string{"label": "mine"}), http.StatusNotFound)
	expect(t, other.do(http.MethodDelete, base+"/claude-2", nil), http.StatusNoContent)
	expect(t, other.do(http.MethodGet, base+"/claude-2/login", nil), http.StatusNotFound)
	if a := owner.accountByID("claude-2"); a.State != Connected || a.Label != "Work" {
		t.Fatalf("another user changed the owner's account: %+v", a)
	}
	for _, a := range other.list().Accounts {
		if a.ID == "claude-2" {
			t.Errorf("the owner's second account is in another user's list: %+v", a)
		}
	}

	// Handles that aren't accounts.
	for _, bad := range []string{"claude-1", "claude-10", "claude-s1", "nope"} {
		expect(t, owner.do(http.MethodPost, base+"/"+bad+"/login", map[string]any{}), http.StatusNotFound)
	}

	// Disconnecting the second leaves the first signed in.
	expect(t, owner.do(http.MethodDelete, base+"/claude-2", nil), http.StatusNoContent)
	for _, a := range owner.list().Accounts {
		if a.ID == "claude-2" {
			t.Errorf("a disconnected second account is still listed: %+v", a)
		}
	}
	if a := owner.account(cliworker.Claude); a.State != Connected {
		t.Fatalf("disconnecting the second account touched the first: %+v", a)
	}
}

// A second account whose login is under way is listed, so its sign-in can be shown.
func TestAccountListedWhileItsLoginRuns(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	owner, _ := e.setup()
	owner.startLoginOf("codex-3")
	a := owner.accountByID("codex-3")
	if a.State != Disconnected || a.Login == nil || a.Login.State != cliworker.LoginWaiting || a.Slot != 3 {
		t.Fatalf("account = %+v", a)
	}
}

// A turn runs on the owner's first connected account for the CLI, and a conversation that
// moves to another account starts a new session there with the conversation so far, also on
// an account where it had one before.
func TestTurnOnAnotherAccount(t *testing.T) {
	e := newTurnEnv(t)
	e.owner.connect(cliworker.Claude)
	e.owner.connectAccount("claude-2")
	dirs := e.layout.User(e.owner.id)
	first, second := dirs.Config(cliworker.Claude), dirs.ConfigOf(cliworker.Claude, "2")
	conv := e.conversation(e.owner)
	history := []historyEntry{{Role: "user", Text: "remember zup"}, {Role: "assistant", Text: "ok: zup"}}

	e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "one"})
	if n := len(turnCalls(t, first)); n != 1 {
		t.Fatalf("turns on the first account = %d", n)
	}

	// The first needs a new sign-in: the turn goes to the second, which starts with the
	// conversation so far.
	if err := e.svc.setReconnect(t.Context(), e.accountID(e.owner.id, cliworker.Claude)); err != nil {
		t.Fatal(err)
	}
	if d := done(e.turn(conv, turnBody{Turn: "t2", CLI: "claude", Model: "default", Prompt: "two", History: history})); d.Status != cliworker.StatusOK {
		t.Fatalf("turn on the second account = %+v", d)
	}
	calls := turnCalls(t, second)
	if len(calls) != 1 || !strings.Contains(calls[0].Prompt, "user: remember zup") || !strings.HasSuffix(calls[0].Prompt, "two") {
		t.Fatalf("second account's turns = %+v", calls)
	}
	if slices.Contains(calls[0].Args, "--resume") {
		t.Error("a new account resumed a session")
	}

	// Back on the first: its old session doesn't know the second's part, so a new one starts.
	clitest.SignIn(t, "claude", first)
	expect(t, e.owner.do(http.MethodPost, base+"/claude/check", map[string]any{}), http.StatusOK)
	e.turn(conv, turnBody{Turn: "t3", CLI: "claude", Model: "default", Prompt: "three", History: history})
	calls = turnCalls(t, first)
	if len(calls) != 2 {
		t.Fatalf("first account's turns = %d", len(calls))
	}
	if slices.Contains(calls[1].Args, "--resume") || !strings.Contains(calls[1].Prompt, "user: remember zup") {
		t.Errorf("back on the first account: args %v, prompt %q", calls[1].Args, calls[1].Prompt)
	}
	// Then it resumes, as the conversation's last session.
	e.turn(conv, turnBody{Turn: "t4", CLI: "claude", Model: "default", Prompt: "four", History: history})
	calls = turnCalls(t, first)
	if last := calls[len(calls)-1]; !slices.Contains(last.Args, "--resume") || last.Prompt != "four" {
		t.Errorf("the next turn didn't resume: args %v, prompt %q", last.Args, last.Prompt)
	}
}

// turnCalls returns the fake CLI's turns (not status checks) recorded in configDir.
func turnCalls(t *testing.T, configDir string) []clitest.Call {
	t.Helper()
	var out []clitest.Call
	for _, c := range clitest.Calls(t, configDir) {
		if len(c.Args) > 0 && c.Args[0] == "-p" {
			out = append(out, c)
		}
	}
	return out
}
