//go:build unix

package cliaccounts

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

const switchingPath = "/api/me/cli-switching"

func (c *client) setSwitching(body any) switchingState {
	c.e.t.Helper()
	rec := c.do(http.MethodPut, switchingPath, body)
	expect(c.e.t, rec, http.StatusOK)
	return decode[switchingState](c.e.t, rec)
}

func TestSwitchingAPI(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	owner, ws := e.setup()
	other := e.invite(owner, ws, "other@example.com")
	p := newProvider(t)
	shared := owner.addShared(ws, cliworker.Codex, p.URL)
	owner.connect(cliworker.Claude)
	owner.connectAccount("claude-2")

	rec := owner.do(http.MethodGet, switchingPath, nil)
	expect(t, rec, http.StatusOK)
	st := decode[switchingState](t, rec)
	if st.Enabled || len(st.Queue) != 0 || len(st.Available) != 3 {
		t.Fatalf("state = %+v", st)
	}
	st = owner.setSwitching(map[string]any{"enabled": true, "queue": []map[string]any{
		{"account": "claude-2"}, {"shared": shared.ID}, {"account": "claude"},
	}})
	ids := []string{}
	for _, q := range st.Queue {
		ids = append(ids, q.Kind+":"+q.ID)
	}
	if !st.Enabled || strings.Join(ids, " ") != "own:claude-2 shared:"+itoa(shared.ID)+" own:claude" {
		t.Fatalf("queue = %v (%+v)", ids, st)
	}

	for _, bad := range []map[string]any{{"account": "codex"}, {"shared": shared.ID + 100}, {"account": "nope"}} {
		expect(t, owner.do(http.MethodPut, switchingPath, map[string]any{"queue": []map[string]any{bad}}), http.StatusNotFound)
	}
	expect(t, other.do(http.MethodPut, switchingPath, map[string]any{"queue": []map[string]any{{"account": "claude-2"}}}), http.StatusNotFound)
	expect(t, owner.do(http.MethodPut, switchingPath, map[string]any{"queue": []map[string]any{{"account": "claude"}, {"account": "claude"}}}), http.StatusBadRequest)
	expect(t, owner.do(http.MethodPut, switchingPath, map[string]any{"queue": []map[string]any{{"account": "claude", "shared": shared.ID}}}), http.StatusBadRequest)
	if st := decode[switchingState](t, other.do(http.MethodGet, switchingPath, nil)); st.Enabled || len(st.Queue) != 0 {
		t.Fatalf("another user's state = %+v", st)
	}
	outsider := e.invite(owner, ws, "outsider@example.com")
	if _, err := e.db.Exec(`DELETE FROM memberships WHERE user_id = ?`, outsider.id); err != nil {
		t.Fatal(err)
	}
	expect(t, outsider.do(http.MethodPut, switchingPath, map[string]any{"queue": []map[string]any{{"shared": shared.ID}}}), http.StatusNotFound)

	expect(t, owner.do(http.MethodDelete, base+"/claude-2", nil), http.StatusNoContent)
	st = decode[switchingState](t, owner.do(http.MethodGet, switchingPath, nil))
	if len(st.Queue) != 2 || st.Queue[0].Kind != "shared" {
		t.Fatalf("after a disconnect: %+v", st.Queue)
	}
	if st := owner.setSwitching(map[string]any{"enabled": false}); st.Enabled || len(st.Queue) != 2 {
		t.Fatalf("off: %+v", st)
	}
}

func TestLimitedUntil(t *testing.T) {
	now := time.Now()
	soon, past := now.Add(time.Hour), now.Add(-time.Minute)
	read := now.Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	old := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	full, half := 100.0, 50.0
	for _, c := range []struct {
		name   string
		limits []cliworker.Limit
		at     string
		want   bool
	}{
		{"rejected until later", []cliworker.Limit{{Window: "five_hour", Status: "rejected", ResetsAt: &soon}}, read, true},
		{"rejected, reset passed", []cliworker.Limit{{Window: "five_hour", Status: "rejected", ResetsAt: &past}}, read, false},
		{"message only, lately", []cliworker.Limit{{Window: "plan", Status: "rejected"}}, read, true},
		{"message only, long ago", []cliworker.Limit{{Window: "plan", Status: "rejected"}}, old, false},
		{"codex at 100%", []cliworker.Limit{{Window: "primary", UsedPercent: &full, ResetsAt: &soon}}, read, true},
		{"codex at 50%", []cliworker.Limit{{Window: "primary", UsedPercent: &half, ResetsAt: &soon}}, read, false},
		{"a warning", []cliworker.Limit{{Window: "five_hour", Status: "allowed_warning", ResetsAt: &soon}}, read, false},
	} {
		if got, _ := limitedUntil(c.limits, &c.at, now); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

type modelCalls struct {
	mu     sync.Mutex
	clis   []cliworker.CLI
	refs   []string
	refuse map[string]bool
}

func (m *modelCalls) set(_ context.Context, _ int64, provider, model string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refuse[provider] {
		return fmt.Errorf("unknown model")
	}
	m.refs = append(m.refs, provider+"/"+model)
	if cli, ok := strings.CutPrefix(provider, cliProviderPrefix); ok {
		m.clis = append(m.clis, cliworker.CLI(cli))
	}
	return nil
}

func (m *modelCalls) list() []cliworker.CLI {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]cliworker.CLI(nil), m.clis...)
}

func switchEnv(t *testing.T, enabled bool) (*turnEnv, int64, *modelCalls) {
	t.Helper()
	e := newTurnEnv(t)
	models := &modelCalls{}
	e.svc.SetModelSetter(models.set)
	e.owner.connect(cliworker.Claude)
	e.owner.connect(cliworker.Codex)
	clitest.WriteScenario(t, e.layout.User(e.owner.id).Config(cliworker.Claude), clitest.Scenario{Fail: "You've hit your usage limit · resets 3pm"})
	e.owner.setSwitching(map[string]any{"enabled": enabled, "queue": []map[string]any{{"account": "claude"}, {"account": "codex"}}})
	return e, e.conversation(e.owner), models
}

var history = []historyEntry{{Role: "user", Text: "remember zup"}, {Role: "assistant", Text: "ok: zup"}}

func TestSwitchOnALimit(t *testing.T) {
	e, conv, models := switchEnv(t, true)
	events := e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "opus", Prompt: "do it", History: history})
	d := done(events)
	if d.Status != cliworker.StatusOK || !strings.HasPrefix(d.Text, "echo: ") {
		t.Fatalf("done = %+v\n%+v", d, events)
	}
	last, notice := 0, ""
	for _, ev := range events {
		if ev.Seq != 0 && ev.Seq <= last {
			t.Fatalf("events out of order at %+v: %+v", ev, events)
		}
		if ev.Seq != 0 {
			last = ev.Seq
		}
		if ev.Type == cliworker.EventNotice && strings.Contains(ev.Text, "continuing on") {
			notice = ev.Text
		}
	}
	if notice != "Claude Code reached its usage limit: continuing on Codex" {
		t.Errorf("notice = %q", notice)
	}
	calls := clitest.Calls(t, e.layout.User(e.owner.id).Config(cliworker.Codex))
	var turn clitest.Call
	for _, c := range calls {
		if len(c.Args) > 0 && c.Args[0] == "exec" {
			turn = c
		}
	}
	for _, want := range []string{"The previous agent (Claude Code) stopped part-way", "reached its usage limit", "user: remember zup", "do it"} {
		if !strings.Contains(turn.Prompt, want) {
			t.Errorf("Codex's prompt lacks %q:\n%s", want, turn.Prompt)
		}
	}
	for _, a := range turn.Args {
		if a == "opus" {
			t.Error("Claude's model went to Codex")
		}
	}
	if got := models.list(); len(got) != 1 || got[0] != cliworker.Codex {
		t.Errorf("model changes = %v", got)
	}
	if a := e.owner.account(cliworker.Claude); len(a.Limits) == 0 || a.Limits[0].Status != "rejected" {
		t.Errorf("Claude's limits = %+v", a.Limits)
	}
	if d := done(e.turn(conv, turnBody{Turn: "t2", CLI: "claude", Model: "default", Prompt: "again"})); d.Status != cliworker.StatusOK {
		t.Fatalf("next turn = %+v", d)
	}
	if n := len(turnCalls(t, e.layout.User(e.owner.id).Config(cliworker.Claude))); n != 1 {
		t.Errorf("Claude ran %d turns: the limited account was tried again", n)
	}

	again := e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "opus", Prompt: "do it", History: history})
	if done(again).Status != cliworker.StatusOK || done(again).Text != d.Text {
		t.Fatalf("re-attached = %+v", done(again))
	}
	if len(again) != len(events) || done(again).Seq != done(events).Seq {
		t.Errorf("re-attached: %d events (last %d), first time %d (last %d)", len(again), done(again).Seq, len(events), done(events).Seq)
	}
}

func TestNoSwitchWhenOff(t *testing.T) {
	e, conv, models := switchEnv(t, false)
	d := done(e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it"}))
	if d.Status != cliworker.StatusFailed || !strings.Contains(d.Text, "usage limit") {
		t.Fatalf("done = %+v", d)
	}
	for _, c := range clitest.Calls(t, e.layout.User(e.owner.id).Config(cliworker.Codex)) {
		if len(c.Args) > 0 && c.Args[0] == "exec" {
			t.Fatal("Codex ran a turn with switching off")
		}
	}
	if len(models.list()) != 0 {
		t.Error("the model changed with switching off")
	}
}

func TestSwitchOnAnExpiredLogin(t *testing.T) {
	e, conv, _ := switchEnv(t, true)
	clitest.WriteScenario(t, e.layout.User(e.owner.id).Config(cliworker.Claude), clitest.Scenario{Expired: true})
	events := e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it"})
	if d := done(events); d.Status != cliworker.StatusOK {
		t.Fatalf("done = %+v\n%+v", d, events)
	}
	if a := e.owner.account(cliworker.Claude); a.State != Reconnect {
		t.Errorf("Claude's state = %s", a.State)
	}
}

func TestStartPastALimitedAccount(t *testing.T) {
	e := newTurnEnv(t)
	e.owner.connect(cliworker.Claude)
	e.owner.connectAccount("claude-2")
	e.owner.setSwitching(map[string]any{"enabled": true, "queue": []map[string]any{{"account": "claude"}, {"account": "claude-2"}}})
	resets := time.Now().Add(2 * time.Hour)
	if err := e.svc.recordLimits(t.Context(), e.accountID(e.owner.id, cliworker.Claude), []cliworker.Limit{{Window: "five_hour", Status: "rejected", ResetsAt: &resets}}); err != nil {
		t.Fatal(err)
	}
	conv := e.conversation(e.owner)
	events := e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "hello"})
	if d := done(events); d.Status != cliworker.StatusOK {
		t.Fatalf("done = %+v", d)
	}
	if n := len(turnCalls(t, e.layout.User(e.owner.id).Config(cliworker.Claude))); n != 0 {
		t.Errorf("the limited account ran %d turns", n)
	}
	if n := len(turnCalls(t, e.layout.User(e.owner.id).ConfigOf(cliworker.Claude, "2"))); n != 1 {
		t.Errorf("the second account ran %d turns", n)
	}
	found := false
	for _, ev := range events {
		if ev.Type == cliworker.EventNotice && strings.Contains(ev.Text, "is at its usage limit until") && strings.Contains(ev.Text, "Claude Code (account 2)") {
			found = true
		}
	}
	if !found {
		t.Errorf("no notice: %+v", events)
	}
}

func TestQueueOrdersTheAccounts(t *testing.T) {
	e := newTurnEnv(t)
	e.owner.connect(cliworker.Grok)
	e.owner.connectAccount("grok-2")
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "grok-2"}, {"account": "grok"}}})
	conv := e.conversation(e.owner)
	e.turn(conv, turnBody{Turn: "t1", CLI: "grok", Model: "default", Prompt: "hello"})
	if n := len(clitest.Calls(t, e.layout.User(e.owner.id).ConfigOf(cliworker.Grok, "2"))); n == 0 {
		t.Error("the first account of the queue didn't run the turn")
	}
}

func TestSwitchSkipsAnotherWorkspacesAccount(t *testing.T) {
	e, conv, _ := switchEnv(t, true)
	p := newProvider(t)
	other := e.secondWorkspace(e.owner)
	foreign := e.owner.addShared(other, cliworker.Grok, p.URL)
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, {"shared": foreign.ID}, {"account": "codex"}}})
	events := e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it"})
	if d := done(events); d.Status != cliworker.StatusOK {
		t.Fatalf("done = %+v", d)
	}
	for _, ev := range events {
		if ev.Type == cliworker.EventNotice && strings.Contains(ev.Text, "Grok") {
			t.Fatalf("switched to another workspace's account: %+v", ev)
		}
	}
	if len(p.requests()) != 0 {
		t.Error("another workspace's key was used")
	}
}

func TestStopEndsTheChain(t *testing.T) {
	e, conv, _ := switchEnv(t, true)
	codex := e.layout.User(e.owner.id).Config(cliworker.Codex)
	clitest.WriteScenario(t, codex, clitest.Scenario{SleepMS: 20000})
	turnDone := make(chan struct{})
	go func() {
		defer close(turnDone)
		e.post(conv, "/cli/turn", turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it"})
	}()
	t.Cleanup(func() { <-turnDone })
	id := fmt.Sprintf("c%d-t1", conv)
	eventually(t, "the switch to Codex", func() bool { _, ok := e.worker.Turn(id + "-s1"); return ok })
	code, _, _ := e.post(conv, "/cli/stop", map[string]string{"turn": "t1"})
	if code != http.StatusNoContent {
		t.Fatalf("stop = %d", code)
	}
	next, _ := e.worker.Turn(id + "-s1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for seen := 0; ; {
		events, done, err := next.Wait(ctx, seen)
		if err != nil {
			t.Fatal("the switched turn didn't stop")
		}
		if done {
			if last := events[len(events)-1]; last.Status != cliworker.StatusStopped {
				t.Fatalf("switched turn ended %+v", last)
			}
			break
		}
		seen = events[len(events)-1].Seq
	}
	if !e.svc.isStopped(id) {
		t.Error("the turn isn't marked stopped")
	}
}
