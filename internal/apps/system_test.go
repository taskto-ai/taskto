package apps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	"taskto.ai/taskto/internal/access"
)

var testSystem = SystemActor{Kind: SystemKindIntegration, ID: 7}

func sysWrite(s access.Space, a App, id int64, amount any) SystemWrite {
	return SystemWrite{
		Actor: testSystem, SpaceID: s.ID, AppID: a.ID, RecordID: id, Action: "payment.succeeded",
		Declared: []string{"amount"}, Set: map[string]any{"amount": amount},
	}
}

func TestSystemWriteChangesDeclaredFieldsAndIsInTheHistory(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	id, base := w.idOf(w.alice, s, a, "lunch")
	ctx := context.Background()

	res, err := w.e.s.SystemWrite(ctx, sysWrite(s, a, id, 25.0))
	if err != nil || !res.Changed || res.Revision != base+1 {
		t.Fatalf("system write = %+v, %v; want a change to revision %d", res, err, base+1)
	}
	if revision, amount := w.revisionOf(w.alice, s, a, id); revision != base+1 || amount != 25.0 {
		t.Fatalf("the record after the write: revision %d, amount %v", revision, amount)
	}

	h := decode[History](t, w.history(w.alice, s, a, id))
	if got := operations(h); got != "2:update 1:insert" {
		t.Fatalf("history = %q", got)
	}
	latest, first := h.History[0], h.History[1]
	if latest.By == nil || latest.By.System == nil || *latest.By.System != (SystemAuthor{Kind: "integration", ID: 7}) || latest.By.ID != 0 || latest.By.Name != "" {
		t.Fatalf("the system write's author = %+v", latest.By)
	}
	if latest.Action != "payment.succeeded" || latest.Data["amount"] != 25.0 || latest.Data["description"] != "lunch" {
		t.Fatalf("the system write's version = %+v", latest)
	}
	if first.By == nil || first.By.System != nil || first.By.Name != "Alice" {
		t.Fatalf("the person's first version = %+v", first.By)
	}
	if h.Record.UpdatedBy == nil || h.Record.UpdatedBy.System == nil || h.Record.UpdatedAction != "payment.succeeded" {
		t.Fatalf("the record's updated_by = %+v, action %q", h.Record.UpdatedBy, h.Record.UpdatedAction)
	}
	if h.Record.CreatedBy == nil || h.Record.CreatedBy.Name != "Alice" {
		t.Fatalf("the record's created_by = %+v", h.Record.CreatedBy)
	}
	var actor, updater *int64
	var actorSystem, updatedSystem *string
	if err := w.e.db.QueryRow(`SELECT actor, actor_system FROM app_row_history WHERE row_id = ? AND revision = ?`, id, base+1).Scan(&actor, &actorSystem); err != nil {
		t.Fatal(err)
	}
	if err := w.e.db.QueryRow(`SELECT updated_by, updated_system FROM app_rows WHERE id = ?`, id).Scan(&updater, &updatedSystem); err != nil {
		t.Fatal(err)
	}
	if actor != nil || updater != nil || actorSystem == nil || *actorSystem != "integration:7" || updatedSystem == nil || *updatedSystem != "integration:7" {
		t.Fatalf("columns: actor %v, actor_system %v, updated_by %v, updated_system %v", actor, actorSystem, updater, updatedSystem)
	}
	list := decode[Records](t, w.records(w.alice, s, a))
	if len(list.Records) != 1 || list.Records[0].UpdatedBy == nil || list.Records[0].UpdatedBy.System == nil {
		t.Fatalf("records = %+v", list.Records)
	}

	res, err = w.e.s.SystemWrite(ctx, sysWrite(s, a, id, 25.0))
	if err != nil || res.Changed || res.Revision != base+1 {
		t.Fatalf("a write that changes nothing = %+v, %v", res, err)
	}
	if got := operations(decode[History](t, w.history(w.alice, s, a, id))); got != "2:update 1:insert" {
		t.Fatalf("history after a replay = %q", got)
	}

	expect(t, w.act(w.alice, s, a, "edit_expense", map[string]any{"id": id, "amount": "30"}, rev(base+1)), http.StatusOK)
	h = decode[History](t, w.history(w.alice, s, a, id))
	if h.Record.UpdatedBy == nil || h.Record.UpdatedBy.System != nil || h.Record.UpdatedBy.Name != "Alice" || h.History[0].By.System != nil {
		t.Fatalf("after a person's edit: updated_by %+v, latest %+v", h.Record.UpdatedBy, h.History[0].By)
	}
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": id}, nil), http.StatusOK)
	if _, err := w.e.s.SystemWrite(ctx, sysWrite(s, a, id, 1.0)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a system write to a deleted record = %v, want ErrNotFound", err)
	}
}

func TestSystemWriteRefusesWhatIsNotDeclared(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	id, base := w.idOf(w.alice, s, a, "lunch")
	ctx := context.Background()

	for name, mutate := range map[string]func(*SystemWrite){
		"an unrelated field of the app":         func(sw *SystemWrite) { sw.Set = map[string]any{"description": "paid"} },
		"one declared field and one that isn't": func(sw *SystemWrite) { sw.Set["description"] = "paid" },
		"a field the app doesn't have":          func(sw *SystemWrite) { sw.Declared, sw.Set = []string{"ghost"}, map[string]any{"ghost": "x"} },
		"nothing declared":                      func(sw *SystemWrite) { sw.Declared = nil },
	} {
		sw := sysWrite(s, a, id, 99.0)
		mutate(&sw)
		if _, err := w.e.s.SystemWrite(ctx, sw); !errors.Is(err, ErrSystemField) {
			t.Fatalf("%s: err = %v, want ErrSystemField", name, err)
		}
	}
	if revision, amount := w.revisionOf(w.alice, s, a, id); revision != base || amount != 10.0 {
		t.Fatalf("a refused write changed the record: revision %d, amount %v", revision, amount)
	}
	if got := operations(decode[History](t, w.history(w.alice, s, a, id))); got != "1:insert" {
		t.Fatalf("history after refused writes = %q", got)
	}

	w.dropLiveField(a, "amount")
	if _, err := w.e.s.SystemWrite(ctx, sysWrite(s, a, id, 99.0)); !errors.Is(err, ErrSystemField) {
		t.Fatalf("a write to a field the live release dropped = %v, want ErrSystemField", err)
	}
}

func TestSystemWriteStillFitsTheCollection(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	id, base := w.idOf(w.alice, s, a, "lunch")
	ctx := context.Background()

	if _, err := w.e.s.SystemWrite(ctx, sysWrite(s, a, id, "not a number")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a text for a number = %v, want ErrInvalid", err)
	}
	if _, err := w.e.s.SystemWrite(ctx, sysWrite(s, a, id, nil)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("clearing a required field = %v, want ErrInvalid", err)
	}
	for name, sw := range map[string]SystemWrite{
		"no actor":            {SpaceID: s.ID, AppID: a.ID, RecordID: id, Action: "payment.succeeded", Declared: []string{"amount"}, Set: map[string]any{"amount": 1.0}},
		"an action in words":  func() SystemWrite { x := sysWrite(s, a, id, 1.0); x.Action = "Payment received!"; return x }(),
		"no action":           func() SystemWrite { x := sysWrite(s, a, id, 1.0); x.Action = ""; return x }(),
		"nothing to set":      func() SystemWrite { x := sysWrite(s, a, id, 1.0); x.Set = nil; return x }(),
		"a zero revision":     func() SystemWrite { x := sysWrite(s, a, id, 1.0); x.Expected = rev(0); return x }(),
		"a different kind":    func() SystemWrite { x := sysWrite(s, a, id, 1.0); x.Actor.Kind = "user"; return x }(),
		"an actor without id": func() SystemWrite { x := sysWrite(s, a, id, 1.0); x.Actor.ID = 0; return x }(),
	} {
		if _, err := w.e.s.SystemWrite(ctx, sw); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if revision, amount := w.revisionOf(w.alice, s, a, id); revision != base || amount != 10.0 {
		t.Fatalf("a refused write changed the record: revision %d, amount %v", revision, amount)
	}
}

func TestSystemWriteStaysInItsSpaceAndApp(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	id, base := w.idOf(w.alice, s, a, "lunch")
	ctx := context.Background()
	notFound := func(what string, sw SystemWrite) {
		t.Helper()
		if _, err := w.e.s.SystemWrite(ctx, sw); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: err = %v, want ErrNotFound", what, err)
		}
	}

	notFound("the app named with another space", sysWrite(w.old, a, id, 99.0))
	notFound("the app named with another workspace's space", sysWrite(w.otherShared, a, id, 99.0))
	notFound("the app named with a private space", sysWrite(w.alicePrivate, a, id, 99.0))
	notFound("another space's app", sysWrite(s, w.aliceApp, id, 99.0))
	bobRow := int64(0)
	if err := w.e.db.QueryRow(`SELECT id FROM app_rows WHERE app_id = ?`, w.bobApp.ID).Scan(&bobRow); err != nil {
		t.Fatal(err)
	}
	notFound("a record of another app", sysWrite(w.bobPrivate, a, bobRow, 99.0))
	notFound("a record of another app in the same space", sysWrite(s, a, bobRow, 99.0))
	notFound("a record that doesn't exist", sysWrite(s, a, 99999, 99.0))
	if res, err := w.e.s.SystemWrite(ctx, sysWrite(s, a, id, 99.0)); err != nil || res.Revision != base+1 {
		t.Fatalf("the right space and app: %+v, %v", res, err)
	}
	var untouched float64
	if err := w.e.db.QueryRow(`SELECT json_extract(data, '$.amount') FROM app_rows WHERE id = ?`, bobRow).Scan(&untouched); err != nil || untouched != 2 {
		t.Fatalf("another app's record = %v, %v", untouched, err)
	}

	oldRow := int64(0)
	if err := w.e.db.QueryRow(`SELECT id FROM app_rows WHERE app_id = ?`, w.oldApp.ID).Scan(&oldRow); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.s.SystemWrite(ctx, sysWrite(w.old, w.oldApp, oldRow, 99.0)); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("a write to an archived space = %v, want ErrReadOnly", err)
	}
	if _, err := w.e.db.Exec(`UPDATE apps SET archived_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.s.SystemWrite(ctx, sysWrite(s, a, id, 5.0)); !errors.Is(err, ErrOffline) {
		t.Fatalf("a write to an offline app = %v, want ErrOffline", err)
	}
	draft := w.draft(w.alice, s, "Not yet")
	if _, err := w.e.s.SystemWrite(ctx, sysWrite(s, draft, 1, 5.0)); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("a write to an unpublished app = %v, want ErrNotPublished", err)
	}
}

func TestSystemWriteAndRevisionsKeepConflictsWorking(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	id, base := w.idOf(w.alice, s, a, "lunch")
	ctx := context.Background()

	if _, err := w.e.s.SystemWrite(ctx, sysWrite(s, a, id, 25.0)); err != nil {
		t.Fatal(err)
	}
	rec := w.act(w.alice, s, a, "edit_expense", map[string]any{"id": id, "description": "late lunch"}, rev(base))
	expect(t, rec, http.StatusConflict)
	if reply := decode[conflictReply](t, rec); reply.Current["amount"] != 25.0 || reply.Current["_revision"] != float64(base+1) {
		t.Fatalf("the conflict = %s", rec.Body)
	}
	rec = w.replay(w.alice, s, a, "device-edit", "edit_expense", map[string]any{"id": id, "amount": "3"}, rev(base))
	expect(t, rec, http.StatusConflict)
	if revision, amount := w.revisionOf(w.alice, s, a, id); revision != base+1 || amount != 25.0 {
		t.Fatalf("a refused edit changed the record: revision %d, amount %v", revision, amount)
	}
	expect(t, w.act(w.alice, s, a, "edit_expense", map[string]any{"id": id, "description": "late lunch"}, rev(base+1)), http.StatusOK)
	stale := sysWrite(s, a, id, 40.0)
	stale.Expected = rev(base + 1)
	if _, err := w.e.s.SystemWrite(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("a system write on a stale revision = %v, want ErrConflict", err)
	}
	res, err := w.e.s.SystemWrite(ctx, sysWrite(s, a, id, 40.0))
	if err != nil || res.Revision != base+3 {
		t.Fatalf("a system write on the current record = %+v, %v", res, err)
	}
	items := w.items(w.alice, s, a)
	if len(items) != 1 || items[0]["description"] != "late lunch" || items[0]["amount"] != 40.0 {
		t.Fatalf("the person's change and the system's: %v", items)
	}
	if got := operations(decode[History](t, w.history(w.alice, s, a, id))); got != "4:update 3:update 2:update 1:insert" {
		t.Fatalf("history = %q", got)
	}
}

func TestSystemWriteQueuesNoEffects(t *testing.T) {
	f := newFX(t)
	f.mustSubmit(f.alice, "op-fail-0001", "East gate", "fail")
	id := f.next
	f.work()
	queued := f.count(`SELECT count(*) FROM app_effects`)
	if queued != 3 || len(f.cards()) != 1 || f.cards()[0].Column != "todo" {
		t.Fatalf("setup: %d effects, cards %+v", queued, f.cards())
	}
	sw := SystemWrite{
		Actor: testSystem, SpaceID: f.shared.ID, AppID: f.app.ID, RecordID: id, Action: "payment.succeeded",
		Declared: []string{"state"}, Set: map[string]any{"state": "resolved"},
	}
	res, err := f.e.s.SystemWrite(f.ctx(), sw)
	if err != nil || !res.Changed {
		t.Fatalf("system write = %+v, %v", res, err)
	}
	f.work()
	if n := f.count(`SELECT count(*) FROM app_effects`); n != queued {
		t.Fatalf("effects after a system write: %d, were %d", n, queued)
	}
	if cards := f.cards(); len(cards) != 1 || cards[0].Column != "todo" {
		t.Fatalf("a system write moved the card: %+v", cards)
	}
}

func (w *world) dropLiveField(a App, field string) {
	w.e.t.Helper()
	var raw string
	var version int
	if err := w.e.db.QueryRow(`SELECT r.definition, r.definition_version FROM apps a JOIN app_releases r ON r.app_id = a.id AND r.number = a.live_release WHERE a.id = ?`, a.ID).Scan(&raw, &version); err != nil {
		w.e.t.Fatal(err)
	}
	var def Definition
	if err := json.Unmarshal([]byte(raw), &def); err != nil {
		w.e.t.Fatal(err)
	}
	def.Collection.Fields = slices.DeleteFunc(def.Collection.Fields, func(f Field) bool { return f.Name == field })
	out, err := json.Marshal(def)
	if err != nil {
		w.e.t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`INSERT INTO app_releases (app_id, number, definition, definition_version, requires, published_at) VALUES (?, 2, ?, ?, '[]', '2026-01-01T00:00:00.000Z')`, a.ID, string(out), version); err != nil {
		w.e.t.Fatal(err)
	}
	if _, err := w.e.db.Exec(`UPDATE apps SET live_release = 2 WHERE id = ?`, a.ID); err != nil {
		w.e.t.Fatal(err)
	}
}
