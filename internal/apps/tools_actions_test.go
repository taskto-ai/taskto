package apps

import (
	"errors"
	"testing"
)

// The agent runs an app's actions through the code the action route runs: the effects they set
// going are done by the same worker, as the user who acted, and say the agent made the change.
func TestAgentActionsSetEffectsGoingAsTheRouteDoes(t *testing.T) {
	f := newFX(t)
	agent := func(u *user) Actor { return Actor{UserID: u.id, Agent: true} }
	run := func(a Actor, name string, ctx map[string]any, op string) (ActionResult, error) {
		return f.e.s.RunAction(f.ctx(), a, f.shared.ID, f.app.ID, ActionRun{Action: name, Context: ctx, Operation: op})
	}

	res, err := run(agent(f.alice), "submit_inspection", map[string]any{"site": "East gate", "result": "fail", "notes": "leaking"}, "agent-op-0001")
	if err != nil || res.Kind != ActionInsert || res.ID != f.next || res.Revision != 1 || !res.Changed || res.EffectsQueued != 3 {
		t.Fatalf("insert = %+v, %v", res, err)
	}
	// Asked again under the same operation, nothing is done twice.
	again, err := run(agent(f.alice), "submit_inspection", map[string]any{"site": "East gate", "result": "fail", "notes": "leaking"}, "agent-op-0001")
	if err != nil || !again.Replayed || again.ID != 0 {
		t.Fatalf("again = %+v, %v", again, err)
	}
	f.work()
	cards := f.cards()
	if len(cards) != 1 || cards[0].Column != "todo" {
		t.Fatalf("cards = %+v", cards)
	}
	if d, err := f.docs.Read(f.ctx(), f.maint, f.east(), ""); err != nil || len(d.Content) == 0 {
		t.Fatalf("document = %v", err)
	}
	// The effects are the user's, made through the agent.
	if n := f.count(`SELECT count(*) FROM app_effects WHERE app_id = ? AND user_id = ? AND acted_by_agent = 1 AND state = ?`, f.app.ID, f.alice.id, StateDone); n != 3 {
		t.Fatalf("%d effects of the agent's, want 3", n)
	}

	// An update moves the card; one that sets nothing new queues nothing.
	up, err := run(agent(f.alice), "resolve", map[string]any{"id": float64(res.ID), "state": "resolved"}, "")
	if err != nil || up.Revision != 2 || !up.Changed || up.EffectsQueued != 1 {
		t.Fatalf("resolve = %+v, %v", up, err)
	}
	f.work()
	if cards = f.cards(); len(cards) != 1 || cards[0].Column != "done" {
		t.Fatalf("cards = %+v", cards)
	}
	noop, err := run(agent(f.alice), "resolve", map[string]any{"id": float64(res.ID), "state": "resolved"}, "")
	if err != nil || noop.Changed || noop.EffectsQueued != 0 || noop.Revision != 2 {
		t.Fatalf("a second resolve = %+v, %v", noop, err)
	}

	// Who may not, may not through the agent: a viewer's change is refused before it is made, and a
	// member who can't see the space is told it isn't there. Nothing is queued by either.
	queued := f.count(`SELECT count(*) FROM app_effects`)
	rows := f.count(`SELECT count(*) FROM app_rows`)
	if _, err := run(agent(f.bob), "submit_inspection", map[string]any{"site": "West gate", "result": "fail"}, ""); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("a viewer's insert: %v", err)
	}
	if _, err := run(agent(f.mallory), "submit_inspection", map[string]any{"site": "West gate", "result": "fail"}, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an outsider's insert: %v", err)
	}
	if _, err := run(agent(f.alice), "nope", nil, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unknown action: %v", err)
	}
	if n := f.count(`SELECT count(*) FROM app_effects`); n != queued {
		t.Fatalf("effects queued by refused calls: %d", n-queued)
	}
	if n := f.count(`SELECT count(*) FROM app_rows`); n != rows {
		t.Fatalf("rows written by refused calls: %d", n-rows)
	}
}

// What the agent is listed is what its user may run, and it is the live release's.
func TestAgentActionsAreListedByTheUsersAccess(t *testing.T) {
	f := newFX(t)
	list, err := f.e.s.Actions(f.ctx(), Actor{UserID: f.alice.id, Agent: true}, f.shared.ID, f.app.ID)
	if err != nil || list.Release != 1 || len(list.Actions) != 4 {
		t.Fatalf("actions = %+v, %v", list, err)
	}
	for _, a := range list.Actions {
		if !a.Allowed {
			t.Errorf("an editor may not run %s", a.Name)
		}
	}
	if first := list.Actions[0]; first.Name != "remove" || first.Kind != ActionDelete {
		t.Fatalf("actions are not in order of name: %+v", list.Actions)
	}
	viewer, err := f.e.s.Actions(f.ctx(), Actor{UserID: f.bob.id, Agent: true}, f.shared.ID, f.app.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range viewer.Actions {
		if a.Allowed {
			t.Errorf("a viewer may run %s", a.Name)
		}
	}
	if _, err := f.e.s.Actions(f.ctx(), Actor{UserID: f.mallory.id, Agent: true}, f.shared.ID, f.app.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an outsider: %v", err)
	}
}
