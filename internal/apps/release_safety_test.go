package apps

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
)

// actAs sends action name with ctx as u from release (nil: none named).
func (w *world) actAs(u *user, s access.Space, a App, release *int64, name string, ctx map[string]any) *httptest.ResponseRecorder {
	w.e.t.Helper()
	ca := action(a, name, ctx)
	ca.Release = release
	return u.do(http.MethodPost, appPath(s, a.ID, "/actions"), ca)
}

func release(n int64) *int64 { return &n }

func bound(f float64) *float64 { return &f }

// An action of an older release writes a row the live collection must accept as it is: its
// limits and required fields are the live ones, though the older screen knows nothing of them.
func TestAnOlderReleasesInsertIsHeldToTheLiveCollection(t *testing.T) {
	w := newWorld(t)
	s, a := w.alicePrivate, w.aliceApp
	cur := w.view(w.alice, s, a)
	// Release 2 narrows the text to 5 characters and the amount to at most 0.5: the row stored
	// under release 1 breaks both, which the publisher acknowledges.
	next := fieldsOf(fieldsOf(*cur.Draft, "description", func(f *Field) { f.MaxLength = 5 }), "amount", func(f *Field) { f.Max = bound(0.5) })
	cur = w.mustPatchDraft(w.alice, s, cur, next)
	expect(t, w.tryPublish(w.alice, s, cur,
		SchemaChange{ChangeFieldNarrowed, "amount"}, SchemaChange{ChangeFieldNarrowed, "description"}), http.StatusOK)
	before := storedRows(t, w.e.db, a.ID)

	for _, tt := range []struct {
		name string
		ctx  map[string]any
		code string
	}{
		{"text past the live limit", map[string]any{"description": "far too long", "amount": "2"}, "app_field_too_long"},
		{"number past the live limit", map[string]any{"description": "ok", "amount": "900"}, "app_field_too_large"},
	} {
		rec := w.actAs(w.alice, s, a, release(1), "add_expense", tt.ctx)
		expect(t, rec, http.StatusUnprocessableEntity)
		if errorCode(t, rec) != tt.code {
			t.Fatalf("%s: %s", tt.name, rec.Body)
		}
		// The live screen is refused the same, and nothing was stored by either.
		rec = w.actAs(w.alice, s, a, nil, "add_expense", tt.ctx)
		expect(t, rec, http.StatusUnprocessableEntity)
	}
	if got := storedRows(t, w.e.db, a.ID); len(got) != len(before) {
		t.Fatalf("rows = %v, were %v", got, before)
	}
	// A row the live collection takes is still taken from the older screen.
	expect(t, w.actAs(w.alice, s, a, release(1), "add_expense", map[string]any{"description": "ok", "amount": "0.4"}), http.StatusOK)
}

// A change of an older release's update is held to the live collection too: the row it
// leaves must fit it.
func TestAnOlderReleasesUpdateIsHeldToTheLiveCollection(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "ok", "2")
	id, _ := w.idOf(w.alice, s, a, "ok")
	cur := w.view(w.alice, s, a)
	cur = w.mustPatchDraft(w.alice, s, cur, fieldsOf(*cur.Draft, "amount", func(f *Field) { f.Max = bound(5) }))
	expect(t, w.tryPublish(w.alice, s, cur), http.StatusOK) // no row is past 5: nothing to acknowledge
	rec := w.actAs(w.alice, s, a, release(1), "edit_expense", map[string]any{"id": id, "amount": "900"})
	expect(t, rec, http.StatusUnprocessableEntity)
	if errorCode(t, rec) != "app_field_too_large" {
		t.Fatalf("update past the live limit = %s", rec.Body)
	}
	expect(t, w.actAs(w.alice, s, a, release(1), "edit_expense", map[string]any{"id": id, "amount": "3"}), http.StatusOK)
}

// Rolling back to a release whose collection is the live one's is not judged by the rows, for
// anyone, so a publisher who may not read them learns nothing of them by trying: a release with
// a bound, a release that only changes the screen, then a rollback to the first gives the same
// answer whatever the stored values are.
func TestRollbackToTheLiveCollectionNeverTellsOfTheRows(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	answers := map[float64]string{}
	for _, k := range []float64{1000, 1} {
		a := w.create(w.alice, s, "Bounded")
		w.add(w.alice, s, a, "lunch", "3")
		// The owner builds and publishes, and reads no record.
		w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppBuilder, access.AppPublisher), grant(w.alice.id, access.AppRoles...))
		cur := w.view(w.owner, s, a)
		if cur.Access.ReadRecords != access.ScopeNone {
			t.Fatalf("the owner reads records: %+v", cur.Access)
		}
		// A publisher who can't read the rows acknowledges every change that could affect one.
		cur = w.mustPatchDraft(w.owner, s, cur, fieldsOf(*cur.Draft, "amount", func(f *Field) { f.Max = bound(k) }))
		rec := w.tryPublish(w.owner, s, cur, SchemaChange{ChangeFieldNarrowed, "amount"})
		expect(t, rec, http.StatusOK)
		cur = decode[App](t, rec)
		// A release that changes only the screen.
		screen := *cur.Draft
		screen.Components = append(slices.Clone(screen.Components), json.RawMessage(`{"id":"note","component":"Text","text":"v3"}`))
		cur = w.mustPatchDraft(w.owner, s, cur, screen)
		rec = w.tryPublish(w.owner, s, cur)
		expect(t, rec, http.StatusOK)
		cur = decode[App](t, rec)
		rec = w.tryRollback(w.owner, s, cur, 2)
		answers[k] = http.StatusText(rec.Code)
		expect(t, rec, http.StatusOK)
	}
	if answers[1000] != answers[1] {
		t.Fatalf("a rollback answers by the stored values: %v", answers)
	}
}

// A rollback to a different collection is still judged by the rows, for who reads them all, and
// refused for who doesn't.
func TestRollbackToAnotherCollectionStillChecksTheRows(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.create(w.alice, s, "Bounded")
	w.add(w.alice, s, a, "lunch", "3")
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppBuilder, access.AppPublisher), grant(w.alice.id, access.AppRoles...))
	cur := w.view(w.alice, s, a)
	cur = w.mustPatchDraft(w.alice, s, cur, fieldsOf(*cur.Draft, "amount", func(f *Field) { f.Max = bound(1) }))
	cur = decode[App](t, w.tryPublish(w.alice, s, cur, SchemaChange{ChangeFieldNarrowed, "amount"}))
	// Back to release 1 (no bound) is fine; forward to 2 again misfits the stored 3.
	cur = decode[App](t, w.tryRollback(w.alice, s, cur, 1))
	rec := w.tryRollback(w.alice, s, cur, 2)
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_release_incompatible" {
		t.Fatalf("a rollback to a collection the rows don't fit = %s", rec.Body)
	}
	expectForbidden(t, w.tryRollback(w.owner, s, w.view(w.owner, s, a), 2), "read_records")
}

// The answer to an action of an older release shows nothing the live release doesn't declare:
// not a field it removed, nor a summary over one.
func TestAnOlderReleasesAnswerShowsOnlyTheLiveFields(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	cur := w.view(w.alice, s, a)
	// Release 2 adds a note and a tip, and sums the tips.
	next := with(with(*cur.Draft, Field{Name: "note", Type: TypeString}), Field{Name: "tip", Type: TypeNumber})
	next.Summaries["tips"] = Summary{Kind: "sum", Field: "tip"}
	cur = w.mustPatchDraft(w.alice, s, cur, next)
	cur = decode[App](t, w.tryPublish(w.alice, s, cur))
	expect(t, w.actAs(w.alice, s, a, nil, "add_expense",
		map[string]any{"description": "dinner", "amount": "5", "note": "bob-private-note", "tip": "7"}), http.StatusOK)
	// Release 3 removes both.
	next = without(without(*cur.Draft, "note"), "tip")
	delete(next.Summaries, "tips")
	cur = w.mustPatchDraft(w.alice, s, cur, next)
	expect(t, w.tryPublish(w.alice, s, cur, SchemaChange{ChangeFieldRemoved, "note"}, SchemaChange{ChangeFieldRemoved, "tip"}), http.StatusOK)

	rec := w.actAs(w.alice, s, a, release(2), "add_expense", map[string]any{"description": "lunch", "amount": "2"})
	expect(t, rec, http.StatusOK)
	for _, leaked := range []string{"bob-private-note", `"note"`, `"tip"`, `"tips"`} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatalf("the answer to release 2's action has %s: %s", leaked, rec.Body)
		}
	}
	// What the live release declares is still drawn, summaries included.
	if !strings.Contains(rec.Body.String(), `"total"`) || !strings.Contains(rec.Body.String(), "dinner") {
		t.Fatalf("the answer lost what the live release shows: %s", rec.Body)
	}
}

// Whether a release exists is told only to a caller who may run actions: the app refuses
// the others the same, for a release that exists and one that doesn't.
func TestNamingAReleaseTellsNothingToWhoMayNotAct(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppBuilder, access.AppPublisher), grant(w.alice.id, access.AppRoles...))
	cur := w.view(w.alice, s, a)
	w.mustPatchDraft(w.alice, s, cur, with(*cur.Draft, Field{Name: "note", Type: TypeString}))
	expect(t, w.tryPublish(w.alice, s, w.view(w.alice, s, a)), http.StatusOK) // release 2 is live; 1 is older
	add := map[string]any{"description": "d", "amount": "1"}
	for _, n := range []int64{1, 2, 99} {
		expectForbidden(t, w.actAs(w.owner, s, a, release(n), "add_expense", add), "submit")
	}
	// Who may act is told, as before.
	expect(t, w.actAs(w.alice, s, a, release(99), "add_expense", add), http.StatusNotFound)
	expect(t, w.actAs(w.alice, s, a, release(1), "add_expense", add), http.StatusOK)
}

// Records, their history and a restore follow the live release, not the draft being edited: a
// field the draft drops is still listed, and a field it requires doesn't stop a restore until
// it is published.
func TestRecordsFollowTheLiveReleaseNotTheDraft(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	w.add(w.alice, s, a, "tea", "2")
	id, _ := w.idOf(w.alice, s, a, "lunch")
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": id}, nil), http.StatusOK)

	cur := w.view(w.alice, s, a)
	draft := without(*cur.Draft, "amount")
	draft.Summaries = map[string]Summary{"n": {Kind: "count"}}
	draft = with(draft, Field{Name: "note", Type: TypeString, Required: true})
	w.mustPatchDraft(w.alice, s, cur, draft)

	for _, rec := range []*httptest.ResponseRecorder{w.records(w.alice, s, a), w.deleted(w.alice, s, a), w.history(w.alice, s, a, id)} {
		expect(t, rec, http.StatusOK)
		if !strings.Contains(rec.Body.String(), `"amount"`) {
			t.Fatalf("a record read leaves out a field the live release declares: %s", rec.Body)
		}
	}
	rec := w.restore(w.alice, s, a, id, nil)
	expect(t, rec, http.StatusOK)
	// Published, the required field is the live one's: the next deleted record can't come back
	// without it.
	expect(t, w.tryPublish(w.alice, s, w.view(w.alice, s, a), SchemaChange{ChangeFieldRemoved, "amount"}, SchemaChange{ChangeFieldRequired, "note"}), http.StatusOK)
	id2, _ := w.idOf(w.alice, s, a, "tea")
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": id2}, nil), http.StatusOK)
	rec = w.restore(w.alice, s, a, id2, nil)
	expect(t, rec, http.StatusBadRequest)
	if errorCode(t, rec) != "app_field_required" {
		t.Fatalf("restore without the live release's required field = %s", rec.Body)
	}
}

// A field the live release retyped keeps its old values stored, unseen: neither a summary in
// the answer to an older release's action, nor the records, their history or the deleted ones
// show them.
func TestValuesOfARetypedFieldStayUnseen(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "salary", "424242")
	w.add(w.alice, s, a, "bonus", "111111")
	id, _ := w.idOf(w.alice, s, a, "salary")
	bonus, _ := w.idOf(w.alice, s, a, "bonus")
	cur := w.view(w.alice, s, a)
	// Release 2 makes the amount text and drops the sum over it.
	next := fieldsOf(*cur.Draft, "amount", func(f *Field) { f.Type, f.Min, f.Max = TypeString, nil, nil })
	next.Summaries = map[string]Summary{"n": {Kind: "count"}}
	cur = w.mustPatchDraft(w.alice, s, cur, next)
	expect(t, w.tryPublish(w.alice, s, cur, SchemaChange{ChangeFieldTypeChanged, "amount"}), http.StatusOK)

	rec := w.actAs(w.alice, s, a, release(1), "delete_expense", map[string]any{"id": bonus})
	expect(t, rec, http.StatusOK)
	reads := []*httptest.ResponseRecorder{rec, w.records(w.alice, s, a), w.deleted(w.alice, s, a), w.history(w.alice, s, a, id)}
	for _, rec := range reads {
		expect(t, rec, http.StatusOK)
		for _, leaked := range []string{"424242", "111111"} {
			if strings.Contains(rec.Body.String(), leaked) {
				t.Fatalf("a retyped field's stored value shows: %s", rec.Body)
			}
		}
	}
}
