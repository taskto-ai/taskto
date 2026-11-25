//go:build unix

package cliaccounts

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agenttools"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
	"taskto.ai/taskto/internal/conversations"
)

// turnEnv is env with the tools socket's /cli/ routes, whose capability is "conv-<id>" here:
// the real one is checked by agenttools, and these tests are about what the routes do with
// the principal it names.
type turnEnv struct {
	*env
	tools http.Handler
	ws    int64
	owner *client
	other *client
}

func newTurnEnv(t *testing.T) *turnEnv {
	t.Helper()
	return newTurnEnvWith(t, nil)
}

// newTurnEnvWith is newTurnEnv with tune changing the worker's configuration.
func newTurnEnvWith(t *testing.T, tune func(*cliworker.Config)) *turnEnv {
	t.Helper()
	e := newEnv(t, LocalUnsafe, tune)
	owner, ws := e.setup()
	other := e.invite(owner, ws, "other@example.com")
	authorize := func(r *http.Request) (conversations.Principal, error) {
		id, err := strconv.ParseInt(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer conv-"), 10, 64)
		if err != nil {
			return conversations.Principal{}, &agenttools.Refusal{Status: http.StatusUnauthorized, Message: "no capability"}
		}
		p, err := conversations.PrincipalOf(r.Context(), e.db, id)
		if errors.Is(err, conversations.ErrInactive) {
			return p, &agenttools.Refusal{Status: http.StatusForbidden, Message: err.Error()}
		}
		return p, err
	}
	poll, wait := waitPoll, waitFor
	waitPoll, waitFor = 20*time.Millisecond, 2*time.Second
	t.Cleanup(func() { waitPoll, waitFor = poll, wait })
	return &turnEnv{env: e, tools: e.svc.TurnRoutes(authorize), ws: ws, owner: owner, other: other}
}

var agentIDs int64

// conversation records a conversation of c's, as the conversation API does.
func (e *turnEnv) conversation(c *client) int64 {
	e.t.Helper()
	agentIDs++
	var id int64
	if err := e.db.QueryRow(`INSERT INTO conversations (workspace_id, owner_id, agent_id) VALUES (?, ?, ?) RETURNING id`,
		e.ws, c.id, agentIDs).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

type turnBody struct {
	Turn    string         `json:"turn"`
	CLI     string         `json:"cli"`
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	History []historyEntry `json:"history,omitempty"`
	Replay  bool           `json:"replay,omitempty"`
}

// post calls a /cli/ route for conversation and returns the status and the NDJSON lines.
func (e *turnEnv) post(conversation int64, path string, body any) (int, []cliworker.Event, string) {
	e.t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	r.Header.Set("Authorization", fmt.Sprintf("Bearer conv-%d", conversation))
	rec := httptest.NewRecorder()
	e.tools.ServeHTTP(rec, r)
	var events []cliworker.Event
	if rec.Header().Get("Content-Type") == "application/x-ndjson" {
		sc := bufio.NewScanner(rec.Body)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var ev cliworker.Event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				e.t.Fatalf("event %q: %v", sc.Text(), err)
			}
			events = append(events, ev)
		}
	}
	return rec.Code, events, rec.Body.String()
}

func (e *turnEnv) turn(conversation int64, b turnBody) []cliworker.Event {
	e.t.Helper()
	code, events, body := e.post(conversation, "/cli/turn", b)
	if code != http.StatusOK || len(events) == 0 || events[len(events)-1].Type != cliworker.EventDone {
		e.t.Fatalf("turn = %d %s", code, body)
	}
	return events
}

func done(events []cliworker.Event) cliworker.Event { return events[len(events)-1] }

func (e *turnEnv) sessionOf(conversation int64, cli string) string {
	e.t.Helper()
	var s string
	err := e.db.QueryRow(`SELECT s.session_id FROM cli_sessions s JOIN cli_accounts a ON a.id = s.account_id
		WHERE s.conversation_id = ? AND a.cli = ?`, conversation, cli).Scan(&s)
	if err != nil {
		return ""
	}
	return s
}

func TestTurnOnTheOwnersAccount(t *testing.T) {
	e := newTurnEnv(t)
	e.owner.connect(cliworker.Claude)
	conv := e.conversation(e.owner)
	events := e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "hello"})
	if d := done(events); d.Status != cliworker.StatusOK || d.Text != "echo: hello" {
		t.Fatalf("done = %+v", d)
	}
	// The session was recorded, and the next turn resumes it.
	session := e.sessionOf(conv, "claude")
	if session == "" {
		t.Fatal("no session recorded")
	}
	events = e.turn(conv, turnBody{Turn: "t2", CLI: "claude", Model: "opus", Prompt: "again"})
	if d := done(events); d.Status != cliworker.StatusOK || !strings.Contains(d.Text, "turn 2 of this session") {
		t.Fatalf("second turn = %+v", d)
	}
	calls := clitest.Calls(t, e.layout.User(e.owner.id).Config(cliworker.Claude))
	if len(calls) != 2 || !contains(calls[0].Args, "--session-id", session) || !contains(calls[1].Args, "--resume", session) || !contains(calls[1].Args, "--model", "opus") {
		t.Fatalf("calls = %+v", calls)
	}
	// Each conversation has a working directory of its own.
	if !strings.HasSuffix(calls[0].Dir, fmt.Sprintf("/c%d", conv)) {
		t.Errorf("working directory = %s", calls[0].Dir)
	}
}

func contains(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// Isolation: a conversation's turn runs on its owner's own login, never anyone else's, and its
// capability reaches only its own turns.
func TestTurnNeverUsesAnotherUsersLogin(t *testing.T) {
	e := newTurnEnv(t)
	e.owner.connect(cliworker.Claude)
	mine := e.conversation(e.owner)
	theirs := e.conversation(e.other)

	events := e.turn(theirs, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "use the owner's login"})
	if d := done(events); d.Status != cliworker.StatusFailed || !strings.Contains(d.Text, "isn't connected") {
		t.Fatalf("the other user's turn = %+v", d)
	}
	if calls := clitest.Calls(t, e.layout.User(e.owner.id).Config(cliworker.Claude)); len(calls) != 0 {
		t.Fatalf("the owner's CLI ran for the other user: %+v", calls)
	}

	// Once connected, the other user's turns run in their own directories.
	e.other.connect(cliworker.Claude)
	e.turn(theirs, turnBody{Turn: "t2", CLI: "claude", Model: "default", Prompt: "mine now"})
	if calls := clitest.Calls(t, e.layout.User(e.owner.id).Config(cliworker.Claude)); len(calls) != 0 {
		t.Fatalf("the owner's CLI ran for the other user: %+v", calls)
	}
	if calls := clitest.Calls(t, e.layout.User(e.other.id).Config(cliworker.Claude)); len(calls) != 1 {
		t.Fatalf("the other user's calls = %+v", calls)
	}

	// A turn of the owner's that the other user's capability names by its ID: not reachable.
	clitest.WriteScenario(t, e.layout.User(e.owner.id).Config(cliworker.Claude), clitest.Scenario{SleepMS: 1500})
	result := make(chan []cliworker.Event, 1)
	go func() {
		_, events, _ := e.post(mine, "/cli/turn", turnBody{Turn: "long", CLI: "claude", Model: "default", Prompt: "slow"})
		result <- events
	}()
	eventually(t, "the owner's turn didn't start", func() bool {
		_, ok := e.worker.Turn(fmt.Sprintf("c%d-long", mine))
		return ok
	})
	if code, _, _ := e.post(theirs, "/cli/stop", map[string]string{"turn": "long"}); code != http.StatusNoContent {
		t.Fatalf("stop = %d", code)
	}
	// Nor can it read it: the same turn ID in another conversation is another turn.
	if code, events, _ := e.post(theirs, "/cli/turn", turnBody{Turn: "long", CLI: "claude", Model: "default", Prompt: "attach"}); code != http.StatusOK || strings.Contains(done(events).Text, "slow") {
		t.Fatalf("the other user's attach = %d %+v", code, events)
	}
	if d := done(<-result); d.Status != cliworker.StatusOK || d.Text != "echo: slow" {
		t.Fatalf("the owner's turn after the other user's stop = %+v", d)
	}
}

func TestStopOwnTurn(t *testing.T) {
	e := newTurnEnv(t)
	e.owner.connect(cliworker.Codex)
	clitest.WriteScenario(t, e.layout.User(e.owner.id).Config(cliworker.Codex), clitest.Scenario{SleepMS: 30_000})
	conv := e.conversation(e.owner)
	result := make(chan []cliworker.Event, 1)
	go func() {
		_, events, _ := e.post(conv, "/cli/turn", turnBody{Turn: "long", CLI: "codex", Model: "default", Prompt: "slow"})
		result <- events
	}()
	eventually(t, "the turn didn't start", func() bool {
		_, ok := e.worker.Turn(fmt.Sprintf("c%d-long", conv))
		return ok
	})
	if code, _, _ := e.post(conv, "/cli/stop", map[string]string{"turn": "long"}); code != http.StatusNoContent {
		t.Fatalf("stop = %d", code)
	}
	select {
	case events := <-result:
		if d := done(events); d.Status != cliworker.StatusStopped {
			t.Fatalf("done = %+v", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stopped turn didn't end")
	}
}

// Codex names its thread itself: it is recorded from its first event, and resumed next time.
func TestCodexThreadRecorded(t *testing.T) {
	e := newTurnEnv(t)
	e.owner.connect(cliworker.Codex)
	clitest.WriteScenario(t, e.layout.User(e.owner.id).Config(cliworker.Codex), clitest.Scenario{RateLimits: true})
	conv := e.conversation(e.owner)
	events := e.turn(conv, turnBody{Turn: "t1", CLI: "codex", Model: "default", Prompt: "hi"})
	var thread string
	for _, ev := range events {
		if ev.Type == cliworker.EventSession {
			thread = ev.Session
		}
	}
	if thread == "" || e.sessionOf(conv, "codex") != thread {
		t.Fatalf("thread %q, recorded %q", thread, e.sessionOf(conv, "codex"))
	}
	if d := done(e.turn(conv, turnBody{Turn: "t2", CLI: "codex", Model: "default", Prompt: "again"})); !strings.Contains(d.Text, "turn 2 of this session") {
		t.Fatalf("second turn = %+v", d)
	}
	// The usage limits it reported are the account's.
	if a := e.owner.account(cliworker.Codex); len(a.Limits) != 2 || a.LimitsAt == nil {
		t.Fatalf("account = %+v", a)
	}
}

// A new session in the middle of a conversation starts with the conversation so far.
func TestHandOff(t *testing.T) {
	e := newTurnEnv(t)
	e.owner.connect(cliworker.Grok)
	conv := e.conversation(e.owner)
	history := []historyEntry{{Role: "user", Text: "remember zup"}, {Role: "assistant", Text: "ok: zup"}, {Role: "toolResult", Text: "skipped"}}
	e.turn(conv, turnBody{Turn: "t1", CLI: "grok", Model: "default", Prompt: "what was it?", History: history})
	e.turn(conv, turnBody{Turn: "t2", CLI: "grok", Model: "default", Prompt: "and now?", History: history})
	calls := clitest.Calls(t, e.layout.User(e.owner.id).Config(cliworker.Grok))
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	if p := calls[0].Prompt; !strings.Contains(p, "user: remember zup\nassistant: ok: zup") || strings.Contains(p, "skipped") || !strings.HasSuffix(p, "what was it?") {
		t.Errorf("first prompt = %q", p)
	}
	// A resumed session has it already.
	if p := calls[1].Prompt; p != "and now?" {
		t.Errorf("resumed prompt = %q", p)
	}
}

// A turn the tool runs again after a restart, which the worker no longer has, resumes the
// session and says so.
func TestReplayAfterAWorkerRestart(t *testing.T) {
	e := newTurnEnv(t)
	e.owner.connect(cliworker.Claude)
	conv := e.conversation(e.owner)
	// The tool started a turn, recorded its session, and the worker went down before the CLI
	// ran: the session exists, the turn doesn't.
	var account int64
	if err := e.db.QueryRow(`SELECT id FROM cli_accounts WHERE user_id = ? AND cli = 'claude'`, e.owner.id).Scan(&account); err != nil {
		t.Fatal(err)
	}
	clitest.WriteScenario(t, e.layout.User(e.owner.id).Config(cliworker.Claude), clitest.Scenario{})
	e.turn(conv, turnBody{Turn: "first", CLI: "claude", Model: "default", Prompt: "start the task"})
	events := e.turn(conv, turnBody{Turn: "lost", CLI: "claude", Model: "default", Prompt: "do the task", Replay: true})
	if events[0].Type != cliworker.EventInterrupted {
		t.Fatalf("events = %+v", events)
	}
	calls := clitest.Calls(t, e.layout.User(e.owner.id).Config(cliworker.Claude))
	last := calls[len(calls)-1]
	if !contains(last.Args, "--resume", e.sessionOf(conv, "claude")) || !strings.Contains(last.Prompt, "interrupted by a restart") {
		t.Fatalf("resumed call = %+v", last)
	}
	// A replay the worker still has is read from its start, not run again.
	before := len(calls)
	events = e.turn(conv, turnBody{Turn: "lost", CLI: "claude", Model: "default", Prompt: "do the task", Replay: true})
	if d := done(events); d.Status != cliworker.StatusOK || len(clitest.Calls(t, e.layout.User(e.owner.id).Config(cliworker.Claude))) != before {
		t.Fatalf("re-attach ran the CLI again: %+v", events)
	}
}

// An expired login: the turn says so and marks the account; /cli/wait answers once it is
// connected again.
func TestExpiredLoginAndReconnect(t *testing.T) {
	e := newTurnEnv(t)
	e.owner.connect(cliworker.Claude)
	cfg := e.layout.User(e.owner.id).Config(cliworker.Claude)
	clitest.WriteScenario(t, cfg, clitest.Scenario{Expired: true})
	conv := e.conversation(e.owner)
	events := e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "hi"})
	if d := done(events); d.Status != cliworker.StatusLoginRequired {
		t.Fatalf("done = %+v", d)
	}
	if a := e.owner.account(cliworker.Claude); a.State != Reconnect {
		t.Fatalf("account = %+v", a)
	}
	// While it is not reconnected, a turn says so at once, without starting the CLI.
	n := len(clitest.Calls(t, cfg))
	if d := done(e.turn(conv, turnBody{Turn: "t2", CLI: "claude", Model: "default", Prompt: "hi"})); d.Status != cliworker.StatusLoginRequired || len(clitest.Calls(t, cfg)) != n {
		t.Fatalf("turn while it waits = %+v", d)
	}
	code, _, body := e.post(conv, "/cli/wait", map[string]string{"cli": "claude"})
	if code != http.StatusOK || !strings.Contains(body, `"reconnect"`) {
		t.Fatalf("wait = %d %s", code, body)
	}
	// The user signs in again; the waiting tool hears it. The wait outlasts the login, which
	// takes a few seconds on a busy machine: with the env's 2 s, the wait could end first and
	// answer "reconnect" (CI's Go job failed now and then, always under -race).
	waitFor = 30 * time.Second
	clitest.WriteScenario(t, cfg, clitest.Scenario{})
	waited := make(chan string, 1)
	go func() {
		_, _, body := e.post(conv, "/cli/wait", map[string]string{"cli": "claude"})
		waited <- body
	}()
	e.owner.connect(cliworker.Claude)
	select {
	case body := <-waited:
		if !strings.Contains(body, `"connected"`) {
			t.Fatalf("wait = %s", body)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("the wait didn't end")
	}
	if d := done(e.turn(conv, turnBody{Turn: "t3", CLI: "claude", Model: "default", Prompt: "hi"})); d.Status != cliworker.StatusOK {
		t.Fatalf("turn after the reconnect = %+v", d)
	}
}

func TestTurnRefusals(t *testing.T) {
	e := newTurnEnv(t)
	conv := e.conversation(e.owner)
	for _, b := range []turnBody{
		{Turn: "", CLI: "claude", Model: "default", Prompt: "x"},
		{Turn: "../x", CLI: "claude", Model: "default", Prompt: "x"},
		{Turn: "t", CLI: "bash", Model: "default", Prompt: "x"},
		{Turn: "t", CLI: "claude", Model: "gpt-9", Prompt: "x"},
		{Turn: "t", CLI: "claude", Model: "default", Prompt: ""},
	} {
		if code, _, _ := e.post(conv, "/cli/turn", b); code != http.StatusBadRequest {
			t.Errorf("%+v: %d", b, code)
		}
	}
	// Without a capability, nothing.
	r := httptest.NewRequest(http.MethodPost, "/cli/turn", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	e.tools.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no capability: %d", rec.Code)
	}
	// An archived conversation can't run turns.
	if _, err := e.db.Exec(`UPDATE conversations SET archived_at = 'now' WHERE id = ?`, conv); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := e.post(conv, "/cli/turn", turnBody{Turn: "t", CLI: "claude", Model: "default", Prompt: "x"}); code != http.StatusForbidden {
		t.Errorf("archived: %d", code)
	}
	if _, err := e.db.Exec(`UPDATE conversations SET archived_at = NULL WHERE id = ?`, conv); err != nil {
		t.Fatal(err)
	}
	// A disabled owner's conversation can't start a turn either, and can once enabled.
	if _, err := e.db.Exec(`UPDATE users SET disabled_at = '2026-01-02T03:04:05.000Z' WHERE id = ?`, e.owner.id); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/cli/turn", "/cli/stop", "/cli/wait"} {
		if code, _, _ := e.post(conv, path, turnBody{Turn: "t", CLI: "claude", Model: "default", Prompt: "x"}); code != http.StatusForbidden {
			t.Errorf("%s for a disabled owner: %d", path, code)
		}
	}
	if _, err := e.db.Exec(`UPDATE users SET disabled_at = NULL WHERE id = ?`, e.owner.id); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := e.post(conv, "/cli/turn", turnBody{Turn: "t", CLI: "claude", Model: "default", Prompt: ""}); code != http.StatusBadRequest {
		t.Errorf("an enabled owner reaches the request's own checks: %d", code)
	}
}

// Local mode with a second user refuses the turn, as it refuses logins.
func TestTurnRefusedWithoutABoundary(t *testing.T) {
	e := newTurnEnv(t)
	e.owner.connect(cliworker.Claude)
	e.svc.mode = Local
	conv := e.conversation(e.owner)
	if d := done(e.turn(conv, turnBody{Turn: "t", CLI: "claude", Model: "default", Prompt: "x"})); d.Status != cliworker.StatusFailed || !strings.Contains(d.Text, "OS boundary") {
		t.Fatalf("done = %+v", d)
	}
}

func TestProvidersListsInstalledCLIs(t *testing.T) {
	e := newTurnEnv(t)
	got := e.svc.Providers()
	if len(got) != 3 || got[0].CLI != "claude" || len(got[0].Models) != 4 || got[0].Models[0].ID != "default" {
		t.Fatalf("providers = %+v", got)
	}
	off := New(e.db, Off, nil)
	if off.Providers() != nil {
		t.Error("providers while off")
	}
}

// The sidecar gone in the middle of a turn: the rest of it is still recorded (here the usage
// limits Codex reports at its end), whether or not the sidecar comes back for it.
func TestTurnRecordedAfterTheClientLeft(t *testing.T) {
	e := newTurnEnv(t)
	e.owner.connect(cliworker.Codex)
	clitest.WriteScenario(t, e.layout.User(e.owner.id).Config(cliworker.Codex), clitest.Scenario{SleepMS: 800, RateLimits: true})
	conv := e.conversation(e.owner)
	raw, _ := json.Marshal(turnBody{Turn: "gone", CLI: "codex", Model: "default", Prompt: "hi"})
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/cli/turn", bytes.NewReader(raw))
	r.Header.Set("Authorization", fmt.Sprintf("Bearer conv-%d", conv))
	served := make(chan struct{})
	go func() {
		defer close(served)
		e.tools.ServeHTTP(httptest.NewRecorder(), r)
	}()
	eventually(t, "the turn didn't start", func() bool {
		_, ok := e.worker.Turn(fmt.Sprintf("c%d-gone", conv))
		return ok
	})
	cancel() // the sidecar is gone
	<-served
	eventually(t, "the limits reported after the client left weren't recorded", func() bool {
		return len(e.owner.account(cliworker.Codex).Limits) == 2
	})
}

// A turn waiting for a Reconnect hears of it at once, not at its next poll.
func TestWaitWakesOnConnect(t *testing.T) {
	e := newTurnEnv(t)
	waitPoll, waitFor = time.Minute, time.Minute
	e.owner.connect(cliworker.Claude)
	if err := e.svc.setReconnect(context.Background(), e.accountID(e.owner.id, cliworker.Claude)); err != nil {
		t.Fatal(err)
	}
	conv := e.conversation(e.owner)
	waited := make(chan string, 1)
	go func() {
		_, _, body := e.post(conv, "/cli/wait", map[string]string{"cli": "claude"})
		waited <- body
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	e.owner.connect(cliworker.Claude)
	select {
	case body := <-waited:
		if !strings.Contains(body, `"connected"`) || time.Since(start) > 10*time.Second {
			t.Fatalf("wait = %s after %s", body, time.Since(start))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the wait didn't wake")
	}
}

// A conversation's turns ask its owner before the CLI acts, unless the owner allowed
// everything in it (docs/agent-safety.md); only the owner sees and answers the questions.
func TestTurnAsksTheOwner(t *testing.T) {
	e := newTurnEnv(t)
	e.owner.connect(cliworker.Codex)
	clitest.WriteScenario(t, e.layout.User(e.owner.id).Config(cliworker.Codex), clitest.Scenario{
		Tool: &clitest.ToolUse{Name: "Bash", Input: map[string]any{"command": "touch proof.txt"}},
	})
	ctx := context.Background()

	conv := e.conversation(e.owner)
	type result struct {
		code   int
		events []cliworker.Event
	}
	res := make(chan result, 1)
	go func() {
		code, events, _ := e.post(conv, "/cli/turn", turnBody{Turn: "ask", CLI: "codex", Model: "default", Prompt: "go"})
		res <- result{code, events}
	}()
	var q cliworker.PermissionRequest
	deadline := time.Now().Add(20 * time.Second)
	for {
		qs, err := e.svc.Questions(ctx, e.owner.id, conv)
		if err != nil {
			t.Fatal(err)
		}
		if len(qs) == 1 {
			q = qs[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no question came")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if q.Tool != "Bash" || q.Input != "touch proof.txt" {
		t.Errorf("question = %+v", q)
	}
	if qs, _ := e.svc.Questions(ctx, e.other.id, conv); len(qs) != 0 {
		t.Errorf("another user sees %+v", qs)
	}
	if err := e.svc.Answer(ctx, e.other.id, conv, q.ID, true); !errors.Is(err, cliworker.ErrNoPermission) {
		t.Errorf("another user's answer: %v", err)
	}
	if err := e.svc.Answer(ctx, e.owner.id, conv, q.ID, true); err != nil {
		t.Fatal(err)
	}
	r := <-res
	if r.code != http.StatusOK || !strings.Contains(done(r.events).Text, "tool Bash: ran") {
		t.Fatalf("turn = %d %+v", r.code, r.events)
	}
	asked := 0
	for _, ev := range r.events {
		if ev.Type == cliworker.EventPermission {
			asked++
		}
	}
	if asked != 2 {
		t.Errorf("permission events = %d, want asked and allowed", asked)
	}

	// The owner allowed everything in another conversation: nothing asks there.
	free := e.conversation(e.owner)
	if _, err := e.db.Exec(`UPDATE conversations SET cli_permissions = 'allow_all' WHERE id = ?`, free); err != nil {
		t.Fatal(err)
	}
	events := e.turn(free, turnBody{Turn: "free", CLI: "codex", Model: "default", Prompt: "go"})
	if !strings.Contains(done(events).Text, "tool Bash: ran") {
		t.Fatalf("allow_all turn = %+v", done(events))
	}
	for _, ev := range events {
		if ev.Type == cliworker.EventPermission {
			t.Errorf("allow_all asked: %+v", ev)
		}
	}
}
