//go:build unix

package cliaccounts

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

func limitedClaudeEnv(t *testing.T, enabled bool, queue ...map[string]any) (*turnEnv, int64, *modelCalls) {
	t.Helper()
	e := newTurnEnv(t)
	models := &modelCalls{}
	e.svc.SetModelSetter(models.set)
	e.owner.connect(cliworker.Claude)
	clitest.WriteScenario(t, e.layout.User(e.owner.id).Config(cliworker.Claude), clitest.Scenario{Fail: "You've hit your usage limit · resets 3pm"})
	e.owner.setSwitching(map[string]any{"enabled": enabled, "queue": append([]map[string]any{{"account": "claude"}}, queue...)})
	return e, e.conversation(e.owner), models
}

func mine(e *turnEnv, name string) string { return fmt.Sprintf("u%d-%s", e.owner.id, name) }

func modelEntry(provider, model string) map[string]any {
	return map[string]any{"provider": provider, "model_id": model}
}

func TestALimitedTurnEndsWithAContinueOnTheQueuedModel(t *testing.T) {
	e, conv, models := limitedClaudeEnv(t, true)
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(mine(e, "api"), "big")}})
	events := e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it", History: history})
	d := done(events)
	if !d.Continue || d.Status != cliworker.StatusFailed {
		t.Fatalf("done = %+v", d)
	}
	for _, want := range []string{"The previous agent (Claude Code) stopped part-way", "reached its usage limit"} {
		if !strings.Contains(d.Note, want) {
			t.Errorf("note lacks %q: %s", want, d.Note)
		}
	}
	notice, last := "", 0
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
	if notice != "Claude Code reached its usage limit: continuing on big" {
		t.Errorf("notice = %q", notice)
	}
	if got := models.refs; len(got) != 1 || got[0] != mine(e, "api")+"/big" {
		t.Errorf("model changes = %v", got)
	}
	again := e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it", History: history})
	if !done(again).Continue || done(again).Note != d.Note || done(again).Seq != d.Seq {
		t.Errorf("re-attached = %+v, first time %+v", done(again), d)
	}
}

func TestNoContinueOnAModelWhenSwitchingIsOff(t *testing.T) {
	e, conv, models := limitedClaudeEnv(t, false)
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(mine(e, "api"), "big")}})
	d := done(e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it"}))
	if d.Continue || d.Note != "" || d.Status != cliworker.StatusFailed || !strings.Contains(d.Text, "usage limit") {
		t.Fatalf("done = %+v", d)
	}
	if len(models.refs) != 0 {
		t.Errorf("the model changed with switching off: %v", models.refs)
	}
}

func TestNoContinueOnAModelWhenTheQueueHoldsNone(t *testing.T) {
	e, conv, models := limitedClaudeEnv(t, true)
	if d := done(e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it"})); d.Continue || len(models.refs) != 0 {
		t.Fatalf("done = %+v, model changes %v", d, models.refs)
	}
}

func TestAnAvailableCLIAccountComesBeforeAModel(t *testing.T) {
	e, conv, models := limitedClaudeEnv(t, true)
	e.owner.connect(cliworker.Codex)
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(mine(e, "api"), "big"), {"account": "codex"}}})
	d := done(e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it"}))
	if d.Continue || d.Status != cliworker.StatusOK {
		t.Fatalf("done = %+v", d)
	}
	for _, ref := range models.refs {
		if strings.Contains(ref, "big") {
			t.Errorf("went to the model first: %v", models.refs)
		}
	}
}

func TestAModelNotUsableInTheConversationsWorkspaceIsSkipped(t *testing.T) {
	e, conv, models := limitedClaudeEnv(t, true)
	other := e.secondWorkspace(e.owner)
	foreign := fmt.Sprintf("ws%d-team", other)
	home := fmt.Sprintf("ws%d-team", e.ws)
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(foreign, "far"), modelEntry(home, "near")}})
	d := done(e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it"}))
	if !d.Continue || !strings.Contains(done([]cliworker.Event{d}).Note, "stopped part-way") {
		t.Fatalf("done = %+v", d)
	}
	if got := models.refs; len(got) != 1 || got[0] != home+"/near" {
		t.Fatalf("model changes = %v", got)
	}
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(foreign, "far")}})
	d = done(e.turn(conv, turnBody{Turn: "t2", CLI: "claude", Model: "default", Prompt: "again"}))
	if d.Continue || len(models.refs) != 1 {
		t.Fatalf("continued on another workspace's model: %+v %v", d, models.refs)
	}
}

func TestAModelTheRuntimeRefusesIsSkipped(t *testing.T) {
	e, conv, models := limitedClaudeEnv(t, true)
	gone, ok := mine(e, "gone"), mine(e, "ok")
	models.refuse = map[string]bool{gone: true}
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(gone, "m"), modelEntry(ok, "m")}})
	d := done(e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it"}))
	if !d.Continue || len(models.refs) != 1 || models.refs[0] != ok+"/m" {
		t.Fatalf("done = %+v, model changes %v", d, models.refs)
	}
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(gone, "m")}})
	d = done(e.turn(conv, turnBody{Turn: "t2", CLI: "claude", Model: "default", Prompt: "again"}))
	if d.Continue || d.Status != cliworker.StatusFailed || !strings.Contains(d.Text, "usage limit") {
		t.Fatalf("done = %+v", d)
	}
}

func TestSwitchingAPIKeepsModelsApartFromAccounts(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	owner, ws := e.setup()
	other := e.invite(owner, ws, "other@example.com")
	outsider := e.invite(owner, ws, "outsider@example.com")
	if _, err := e.db.Exec(`DELETE FROM memberships WHERE user_id = ?`, outsider.id); err != nil {
		t.Fatal(err)
	}
	foreignWS := e.secondWorkspace(other)
	owner.connect(cliworker.Claude)
	put := func(c *client, entries ...map[string]any) int {
		return c.do(http.MethodPut, switchingPath, map[string]any{"queue": entries}).Code
	}
	own := fmt.Sprintf("u%d-api", owner.id)
	st := owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(own, "big"), modelEntry(fmt.Sprintf("ws%d-team", ws), "m"), modelEntry("shared", "m")}})
	if len(st.Queue) != 4 || st.Queue[1].Kind != "model" || st.Queue[1].ID != own || st.Queue[1].ModelID != "big" || st.Queue[0].Kind != "own" {
		t.Fatalf("queue = %+v", st.Queue)
	}
	if got := decode[switchingState](t, other.do(http.MethodGet, switchingPath, nil)); len(got.Queue) != 0 {
		t.Fatalf("another user's queue = %+v", got.Queue)
	}
	for name, c := range map[string]struct {
		who   *client
		entry map[string]any
		want  int
	}{
		"another person's provider":                 {owner, modelEntry(fmt.Sprintf("u%d-api", other.id), "big"), http.StatusNotFound},
		"a workspace the person is not in":          {owner, modelEntry(fmt.Sprintf("ws%d-team", foreignWS), "m"), http.StatusNotFound},
		"a CLI provider":                            {owner, modelEntry("cli-claude", "default"), http.StatusNotFound},
		"a model with no id":                        {owner, map[string]any{"provider": own}, http.StatusBadRequest},
		"an id with no provider":                    {owner, map[string]any{"model_id": "m"}, http.StatusBadRequest},
		"a model and an account":                    {owner, map[string]any{"provider": own, "model_id": "m", "account": "claude"}, http.StatusBadRequest},
		"a model with a shared account":             {owner, map[string]any{"provider": own, "model_id": "m", "shared": 1}, http.StatusBadRequest},
		"a provider of someone outside a workspace": {outsider, modelEntry(fmt.Sprintf("ws%d-team", ws), "m"), http.StatusNotFound},
		"a model with an oversized id":              {owner, modelEntry(own, strings.Repeat("x", maxModelRef+1)), http.StatusNotFound},
	} {
		if got := put(c.who, c.entry); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
	if got := put(owner, modelEntry(own, "big"), modelEntry(own, "big")); got != http.StatusBadRequest {
		t.Errorf("a model twice: %d", got)
	}
	if got := put(owner, modelEntry(own, "big"), modelEntry(own, "small")); got != http.StatusOK {
		t.Errorf("two models of one provider: %d", got)
	}
	if st := decode[switchingState](t, owner.do(http.MethodGet, switchingPath, nil)); len(st.Queue) != 2 {
		t.Errorf("queue after the refusals = %+v", st.Queue)
	}
}
