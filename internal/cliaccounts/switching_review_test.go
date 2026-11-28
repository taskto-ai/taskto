//go:build unix

package cliaccounts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

func TestNoModelHopForAConversationThatStoppedBeingTheOwners(t *testing.T) {
	revocations := map[string]string{
		"the owner left the workspace":  `DELETE FROM memberships WHERE user_id = %d`,
		"the owner was disabled":        `UPDATE users SET disabled_at = strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ', 'now') WHERE id = %d`,
		"the conversation was archived": `UPDATE conversations SET archived_at = strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ', 'now') WHERE owner_id = %d`,
	}
	for name, query := range revocations {
		t.Run(name, func(t *testing.T) {
			e, conv, models := limitedClaudeEnv(t, true)
			e.provide(mine(e, "api"), "big")
			clitest.WriteScenario(t, e.dirs(e.owner.id).Config(cliworker.Claude), clitest.Scenario{Fail: "You've hit your usage limit · resets 3pm", SleepMS: 600})
			e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(mine(e, "api"), "big")}})
			go func() {
				time.Sleep(200 * time.Millisecond)
				_, _ = e.db.Exec(fmt.Sprintf(query, e.owner.id))
			}()
			d := done(e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it"}))
			if d.Continue || d.Note != "" {
				t.Fatalf("done = %+v", d)
			}
			if len(models.refs) != 0 {
				t.Fatalf("model changes after the access went: %v", models.refs)
			}
		})
	}
}

func TestAModelThatLeftTheCatalogIsSkippedAtTheHopWhileTheRuntimeStillHasIt(t *testing.T) {
	removals := map[string]string{
		"the provider was deleted":      `DELETE FROM provider_accounts WHERE name = 'gone'`,
		"the model was taken off it":    `UPDATE provider_accounts SET models = '[]' WHERE name = 'gone'`,
		"the provider changed its list": `UPDATE provider_accounts SET models = '[{"id":"other","name":"other","reasoning":false}]' WHERE name = 'gone'`,
	}
	for name, removal := range removals {
		t.Run(name, func(t *testing.T) {
			e, conv, models := limitedClaudeEnv(t, true)
			gone, ok := mine(e, "gone"), mine(e, "ok")
			e.provide(gone, "m")
			e.provide(ok, "m")
			e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(gone, "m"), modelEntry(ok, "m")}})
			if _, err := e.db.Exec(removal); err != nil {
				t.Fatal(err)
			}
			d := done(e.turn(conv, turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it"}))
			if !d.Continue || len(models.refs) != 1 || models.refs[0] != ok+"/m" {
				t.Fatalf("done = %+v, model changes %v", d, models.refs)
			}
		})
	}
}

type cancelOnOffer struct {
	ModelCatalog
	cancel func()
}

func (c cancelOnOffer) Offers(_ context.Context, provider, model string, user, workspace int64) (bool, error) {
	c.cancel()
	return c.ModelCatalog.Offers(context.Background(), provider, model, user, workspace)
}

func TestNoModelHopWhenTheCallerOfTheTurnIsGone(t *testing.T) {
	e, conv, models := limitedClaudeEnv(t, true)
	e.provide(mine(e, "api"), "big")
	e.owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(mine(e, "api"), "big")}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.catalog = cancelOnOffer{e.svc.catalog, cancel}
	raw, _ := json.Marshal(turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it"})
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/cli/turn", bytes.NewReader(raw))
	r.Header.Set("Authorization", fmt.Sprintf("Bearer conv-%d", conv))
	e.tools.ServeHTTP(httptest.NewRecorder(), r)
	if len(models.refs) != 0 {
		t.Fatalf("the model changed for a caller that was gone: %v", models.refs)
	}
}

func TestTheModelIsTriedWhenTheLastAllowedSwitchExhaustsTheAccounts(t *testing.T) {
	e := newTurnEnv(t)
	models := &modelCalls{}
	e.svc.SetModelSetter(models.set)
	e.provide(mine(e, "api"), "big")
	queue := []map[string]any{}
	for n := 1; n <= maxSlot; n++ {
		handle := refOf(cliworker.Claude, n).handle()
		e.owner.connectAccount(handle)
		a, _ := parseHandle(handle)
		clitest.WriteScenario(t, e.dirs(e.owner.id).ConfigOf(a.cli, a.slot), clitest.Scenario{Fail: "You've hit your usage limit · resets 3pm"})
		queue = append(queue, map[string]any{"account": handle})
	}
	e.owner.setSwitching(map[string]any{"enabled": true, "queue": append(queue, modelEntry(mine(e, "api"), "big"))})
	d := done(e.turn(e.conversation(e.owner), turnBody{Turn: "t1", CLI: "claude", Model: "default", Prompt: "do it"}))
	if !d.Continue || len(models.refs) == 0 || models.refs[len(models.refs)-1] != mine(e, "api")+"/big" {
		t.Fatalf("done = %+v, model changes %v", d, models.refs)
	}
}

func TestStaleModelEntriesDoNotStopTheQueueFromBeingSaved(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	owner, ws := e.setup()
	other := e.secondWorkspace(owner)
	own := fmt.Sprintf("ws%d-team", other)
	e.provide(own, "a", "b")
	e.provide(fmt.Sprintf("ws%d-team", ws), "c")
	owner.connect(cliworker.Claude)
	st := owner.setSwitching(map[string]any{"queue": []map[string]any{{"account": "claude"}, modelEntry(own, "a"), modelEntry(own, "b"), modelEntry(fmt.Sprintf("ws%d-team", ws), "c")}})
	if len(st.Queue) != 4 {
		t.Fatalf("queue = %+v", st.Queue)
	}
	if _, err := e.db.Exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, other, owner.id); err != nil {
		t.Fatal(err)
	}
	got := decode[SwitchingState](t, owner.do(http.MethodGet, switchingPath, nil))
	if len(got.Queue) != 2 || got.Queue[0].Kind != "own" || got.Queue[1].ModelID != "c" {
		t.Fatalf("the queue still shows entries the person can't keep: %+v", got.Queue)
	}
	rest := []map[string]any{{"account": "claude"}, modelEntry(fmt.Sprintf("ws%d-team", ws), "c")}
	if code := owner.do(http.MethodPut, switchingPath, map[string]any{"queue": rest}).Code; code != http.StatusOK {
		t.Fatalf("saving what is shown: %d", code)
	}
	var rows int
	if err := e.db.QueryRow(`SELECT count(*) FROM cli_switch_queue WHERE user_id = ?`, owner.id).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("stored entries = %d %v", rows, err)
	}
}
