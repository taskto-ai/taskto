package apps

import (
	"errors"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/access"
)

// The agent runs an app's actions through the code the action route runs: the effects they set
// going are done by the same worker, as the user who acted, and say the agent made the change.
func TestAgentActionsSetEffectsGoingAsTheRouteDoes(t *testing.T) {
	f := newFX(t)
	agent := func(u *user) Actor { return Actor{UserID: u.id, Agent: true} }
	run := func(a Actor, name string, ctx map[string]any, op string) (ActionResult, error) {
		return f.e.s.RunAction(f.ctx(), a, f.shared.ID, f.app.ID, ActionRun{Action: name, Context: ctx, Operation: op, ExpectedRelease: 1})
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

// publishSecond publishes a second release of the inspections app, in which a failed inspection
// makes only the card (in done), and returns it.
func (f *fx) publishSecond() {
	f.e.t.Helper()
	def := f.inspections()
	submit := def.Actions["submit_inspection"]
	submit.Effects = submit.Effects[:1]
	submit.Effects[0].Column = "done"
	def.Actions["submit_inspection"] = submit
	cur := f.view(f.alice, f.shared, f.app)
	cur = f.mustPatchDraft(f.alice, f.shared, cur, def)
	expect(f.e.t, f.tryPublish(f.alice, f.shared, cur), http.StatusOK)
}

// The agent's action runs from the release it was listed from: one published after the list is not
// run, nor its effects set going, unless the agent listed again; a retry of what was run is still
// answered as the first time was.
func TestAgentActionRunsOnlyFromTheReleaseItWasListedFrom(t *testing.T) {
	f := newFX(t)
	agent := Actor{UserID: f.alice.id, Agent: true}
	failed := map[string]any{"site": "East gate", "result": "fail", "notes": "leaking"}
	run := func(release int64, ctx map[string]any, op string) (ActionResult, error) {
		return f.e.s.RunAction(f.ctx(), agent, f.shared.ID, f.app.ID, ActionRun{Action: "submit_inspection", Context: ctx, Operation: op, ExpectedRelease: release})
	}
	if list, err := f.e.s.Actions(f.ctx(), agent, f.shared.ID, f.app.ID); err != nil || list.Release != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if _, err := run(0, failed, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no release: %v", err)
	}
	first, err := run(1, failed, "agent-op-0001")
	if err != nil || first.EffectsQueued != 3 {
		t.Fatalf("first = %+v, %v", first, err)
	}

	f.publishSecond()
	rows, effects := f.count(`SELECT count(*) FROM app_rows`), f.count(`SELECT count(*) FROM app_effects`)

	// Not run: it was listed from release 1 and the live one has other effects.
	for _, op := range []string{"", "agent-op-0002"} {
		if _, err := run(1, failed, op); !errors.Is(err, ErrReleaseOutdated) {
			t.Fatalf("a new request from release 1 (operation %q): %v", op, err)
		}
	}
	if _, err := run(9, failed, ""); !errors.Is(err, ErrReleaseOutdated) {
		t.Fatalf("a release the app has not had: %v", err)
	}
	if f.count(`SELECT count(*) FROM app_rows`) != rows || f.count(`SELECT count(*) FROM app_effects`) != effects {
		t.Fatal("a refused request changed something")
	}

	// An exact retry of the first request is answered as it was; another under its key is refused.
	again, err := run(1, failed, "agent-op-0001")
	if err != nil || !again.Replayed || again.ID != 0 {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	if _, err := run(1, map[string]any{"site": "West gate", "result": "fail"}, "agent-op-0001"); !errors.Is(err, ErrOperationReused) {
		t.Fatalf("another request under the key: %v", err)
	}
	if f.count(`SELECT count(*) FROM app_rows`) != rows || f.count(`SELECT count(*) FROM app_effects`) != effects {
		t.Fatal("a retry changed something")
	}

	// From the live release, what runs is its: one effect, not three.
	second, err := run(2, failed, "")
	if err != nil || second.EffectsQueued != 1 {
		t.Fatalf("from release 2 = %+v, %v", second, err)
	}
	if n := f.count(`SELECT count(*) FROM app_effects`); n != effects+1 {
		t.Fatalf("%d effects queued by release 2, want 1", n-effects)
	}
}

// effects_queued is what was queued: an effect whose condition the record doesn't meet is not.
func TestAgentActionCountsTheEffectsItQueued(t *testing.T) {
	f := newFX(t)
	agent := Actor{UserID: f.alice.id, Agent: true}
	res, err := f.e.s.RunAction(f.ctx(), agent, f.shared.ID, f.app.ID, ActionRun{
		Action: "submit_inspection", ExpectedRelease: 1, Context: map[string]any{"site": "East gate", "result": "pass"}})
	if err != nil || !res.Changed || res.EffectsQueued != 0 {
		t.Fatalf("a passed inspection = %+v, %v", res, err)
	}
	if n := f.count(`SELECT count(*) FROM app_effects`); n != 0 {
		t.Fatalf("%d effects queued", n)
	}
}

// What the agent is listed is read in one piece: a role lost and a release published while the list
// is being made are not shown as the user's.
func TestAgentActionsAreNotListedAcrossARevokedRole(t *testing.T) {
	f := newFX(t)
	agent := Actor{UserID: f.alice.id, Agent: true}
	f.e.s.hooks.afterActionsAccess = func() {
		f.e.s.hooks.afterActionsAccess = nil
		f.publishSecond()
		if _, err := f.e.db.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, f.shared.ID, f.alice.id); err != nil {
			t.Fatal(err)
		}
	}
	if list, err := f.e.s.Actions(f.ctx(), agent, f.shared.ID, f.app.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a user who lost the space was listed release %d: %v", list.Release, err)
	}

	// A release published alone is simply the one listed.
	f = newFX(t)
	f.e.s.hooks.afterActionsAccess = func() {
		f.e.s.hooks.afterActionsAccess = nil
		f.publishSecond()
	}
	if list, err := f.e.s.Actions(f.ctx(), Actor{UserID: f.alice.id, Agent: true}, f.shared.ID, f.app.ID); err != nil || list.Release != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}

	// A role that only changes is read again, and the list is the new role's.
	f = newFX(t)
	f.e.s.hooks.afterActionsAccess = func() {
		f.e.s.hooks.afterActionsAccess = nil
		if _, err := f.e.db.Exec(`UPDATE space_members SET role = ? WHERE space_id = ? AND user_id = ?`, access.RoleViewer, f.shared.ID, f.alice.id); err != nil {
			t.Fatal(err)
		}
	}
	list, err := f.e.s.Actions(f.ctx(), Actor{UserID: f.alice.id, Agent: true}, f.shared.ID, f.app.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list.Actions {
		if a.Allowed {
			t.Errorf("a viewer, as they became, may run %s", a.Name)
		}
	}
}
