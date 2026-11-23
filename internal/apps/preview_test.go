package apps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
)

// preview has u try a's draft: start it (no rows, no action), or go on from rows with an action.
func (w *world) preview(u *user, s access.Space, a App, req PreviewRequest) *httptest.ResponseRecorder {
	w.e.t.Helper()
	return u.do(http.MethodPost, appPath(s, a.ID, "/preview"), req)
}

func (w *world) mustPreview(u *user, s access.Space, a App, req PreviewRequest) PreviewReply {
	w.e.t.Helper()
	rec := w.preview(u, s, a, req)
	expect(w.e.t, rec, http.StatusOK)
	return decode[PreviewReply](w.e.t, rec)
}

// everythingStored is every table an action of an app could write, as text, for comparing a
// database before and after.
func everythingStored(t *testing.T, e *env) string {
	t.Helper()
	var out strings.Builder
	for _, q := range []string{
		`SELECT id || '|' || app_id || '|' || data || '|' || revision || '|' || coalesce(deleted_at, '') || '|' || updated_at FROM app_rows ORDER BY id`,
		`SELECT app_id || '|' || row_id || '|' || revision || '|' || operation || '|' || data FROM app_row_history ORDER BY app_id, row_id, revision`,
		`SELECT id || '|' || version || '|' || updated_at || '|' || coalesce(live_release, 0) || '|' || draft_differs FROM apps ORDER BY id`,
		`SELECT app_id || '|' || number FROM app_releases ORDER BY app_id, number`,
	} {
		rows, err := e.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			out.WriteString(line + "\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		out.WriteString("--\n")
	}
	return out.String()
}

func jsonString(v any) (string, error) {
	raw, err := json.Marshal(v)
	return string(raw), err
}

// A preview starts on sample rows made from the draft's collection. It never shows a stored row.
func TestPreviewStartsOnSampleRowsNotRealOnes(t *testing.T) {
	w := newWorld(t)
	s, a := w.alicePrivate, w.aliceApp
	// The app has a real row, "alice-secret", which the live app shows.
	if got := w.rowsOf(w.alice, s, a); len(got) != 1 || got[0] != "alice-secret" {
		t.Fatalf("live rows = %v", got)
	}
	reply := w.mustPreview(w.alice, s, a, PreviewRequest{})
	if len(reply.Rows) != sampleRowCount {
		t.Fatalf("sample rows = %+v", reply.Rows)
	}
	for _, r := range reply.Rows {
		if r.Data["description"] == "alice-secret" || r.Revision != 1 {
			t.Fatalf("a sample row is not sample: %+v", r)
		}
	}
	body, err := jsonString(reply)
	if err != nil || strings.Contains(body, "alice-secret") {
		t.Fatalf("the preview shows a real row (%v): %s", err, body)
	}
	// And it draws the same screen the app has: three messages that create it.
	if len(reply.Messages) != 3 || reply.Messages[0]["createSurface"] == nil {
		t.Fatalf("messages = %v", reply.Messages)
	}
	// It starts again the same.
	again := w.mustPreview(w.alice, s, a, PreviewRequest{})
	if a, b := fmt.Sprint(reply.Rows), fmt.Sprint(again.Rows); a != b {
		t.Fatalf("sample rows differ between starts: %s, %s", a, b)
	}
}

// Whatever the preview does, the app's rows, their history, the app and its releases are as they
// were: an insert, an update and a delete run on the sample rows in the reply, and nothing is stored.
func TestPreviewActionsWriteNothing(t *testing.T) {
	w := newWorld(t)
	s, a := w.alicePrivate, w.aliceApp
	before := everythingStored(t, w.e)

	rows := w.mustPreview(w.alice, s, a, PreviewRequest{}).Rows

	// Insert: the sample rows have one more, the app none.
	reply := w.mustPreview(w.alice, s, a, PreviewRequest{Rows: rows,
		Action: &ClientAction{Name: "add_expense", SurfaceID: SurfaceID(a.ID), Context: map[string]any{"description": "Tea", "amount": "3"}}})
	if len(reply.Rows) != len(rows)+1 || reply.Rows[len(rows)].Data["description"] != "Tea" {
		t.Fatalf("after the insert: %+v", reply.Rows)
	}
	if reply.Rows[len(rows)].ID != rows[len(rows)-1].ID+1 {
		t.Fatalf("the new sample row's id = %d", reply.Rows[len(rows)].ID)
	}
	if got := w.rowsOf(w.alice, s, a); len(got) != 1 {
		t.Fatalf("the live app's rows after a preview insert = %v", got)
	}
	// An insert that doesn't fit is refused as a live one is, and changes nothing.
	rec := w.preview(w.alice, s, a, PreviewRequest{Rows: rows,
		Action: &ClientAction{Name: "add_expense", SurfaceID: SurfaceID(a.ID), Context: map[string]any{"amount": "3"}}})
	expect(t, rec, http.StatusUnprocessableEntity)
	if errorCode(t, rec) != "app_field_required" {
		t.Fatalf("a row without its description = %s", rec.Body)
	}

	// Delete: from the sample rows, and a row id that is real but isn't a sample row is not there.
	reply = w.mustPreview(w.alice, s, a, PreviewRequest{Rows: reply.Rows,
		Action: &ClientAction{Name: "delete_expense", SurfaceID: SurfaceID(a.ID), Context: map[string]any{"id": float64(1)}}})
	if len(reply.Rows) != len(rows) {
		t.Fatalf("after the delete: %+v", reply.Rows)
	}
	rec = w.preview(w.alice, s, a, PreviewRequest{Rows: []PreviewRow{{ID: 1, Revision: 1, Data: map[string]any{"description": "x", "amount": float64(1)}}},
		Action: &ClientAction{Name: "delete_expense", SurfaceID: SurfaceID(a.ID), Context: map[string]any{"id": float64(99)}}})
	expect(t, rec, http.StatusUnprocessableEntity)
	if errorCode(t, rec) != "app_row_not_found" {
		t.Fatalf("a row the sample rows lack = %s", rec.Body)
	}

	if after := everythingStored(t, w.e); after != before {
		t.Fatalf("a preview changed what is stored:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// The planner has screens and updates: the same holds for them, and an update changes the
// sample row's revision, not a stored one.
func TestPreviewRunsUpdatesAndNavigationOnSampleRows(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	all, err := Examples()
	if err != nil {
		t.Fatal(err)
	}
	var planner Definition
	for _, ex := range all {
		if ex.Name != "Expense tracker" {
			planner = ex.Definition
		}
	}
	rec := w.alice.do(http.MethodPost, appsPath(s, ""), map[string]any{"name": "Planner", "definition": planner})
	expect(t, rec, http.StatusCreated)
	a := decode[App](t, rec)
	before := everythingStored(t, w.e)

	start := w.mustPreview(w.alice, s, a, PreviewRequest{})
	do := func(rows []PreviewRow, name string, ctx map[string]any) PreviewReply {
		t.Helper()
		return w.mustPreview(w.alice, s, a, PreviewRequest{Rows: rows,
			Action: &ClientAction{Name: name, SurfaceID: SurfaceID(a.ID), Context: ctx}})
	}
	// Navigating to a record shows the sample record.
	opened := do(start.Rows, "open_task", map[string]any{"id": float64(2)})
	if !strings.Contains(fmt.Sprint(opened.Messages), "/record") {
		t.Fatalf("open: %v", opened.Messages)
	}
	// Completing a sample task bumps its revision, in the reply.
	done := do(start.Rows, "complete_task", map[string]any{"id": float64(2), "done": true})
	if done.Rows[1].Revision != 2 || done.Rows[1].Data["done"] != true {
		t.Fatalf("update: %+v", done.Rows)
	}
	// A stale revision is a conflict, as in a live app.
	rev := int64(1)
	rec = w.preview(w.alice, s, a, PreviewRequest{Rows: done.Rows, Action: &ClientAction{
		Name: "complete_task", SurfaceID: SurfaceID(a.ID), Context: map[string]any{"id": float64(2), "done": false}, ExpectedRevision: &rev}})
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_record_conflict" {
		t.Fatalf("stale revision = %s", rec.Body)
	}
	if after := everythingStored(t, w.e); after != before {
		t.Fatalf("a preview changed what is stored:\n%s\n%s", before, after)
	}
}

// Only those who may design or publish see a draft. Anyone else, and anyone who can't read the
// space, is refused, and tells nothing about the app.
func TestPreviewIsForBuildersAndPublishers(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	// bob is a viewer of the shared space: he reads the live app, not its draft.
	expectForbidden(t, w.preview(w.bob, s, a, PreviewRequest{}), "design")
	// A user with only the right to add records may not either.
	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppBuilder), grant(w.owner.id, access.AppUser))
	expectForbidden(t, w.preview(w.owner, s, a, PreviewRequest{}), "design")
	expect(t, w.preview(w.alice, s, a, PreviewRequest{}), http.StatusOK)
	// A publisher without the builder role may look.
	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppBuilder), grant(w.owner.id, access.AppPublisher))
	expect(t, w.preview(w.owner, s, a, PreviewRequest{}), http.StatusOK)

	// A member of the workspace outside the space, a stranger of another workspace, and the
	// space of another workspace's path: not found.
	expect(t, w.preview(w.mallory, s, a, PreviewRequest{}), http.StatusNotFound)
	expect(t, w.preview(w.stranger, s, a, PreviewRequest{}), http.StatusNotFound)
	expect(t, w.preview(w.stranger, in(w.other, s), a, PreviewRequest{}), http.StatusNotFound)
	// Someone else's private space.
	expect(t, w.preview(w.bob, w.alicePrivate, w.aliceApp, PreviewRequest{}), http.StatusNotFound)
	// An app that isn't in the space of the path.
	expect(t, w.preview(w.alice, w.alicePrivate, w.sharedApp, PreviewRequest{}), http.StatusNotFound)
	// A preview is never a way to read the roles or the rows: it takes no other route's rights.
	expect(t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/preview"), nil), http.StatusMethodNotAllowed)
}

// A draft that was never published is tried with the preview, and can't be launched or have an
// action run for real: nothing it does reaches the app's rows.
func TestAnUnpublishedDraftCanOnlyBePreviewed(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.draft(w.alice, s, "Fresh")
	before := everythingStored(t, w.e)

	reply := w.mustPreview(w.alice, s, a, PreviewRequest{})
	reply = w.mustPreview(w.alice, s, a, PreviewRequest{Rows: reply.Rows,
		Action: &ClientAction{Name: "add_expense", SurfaceID: SurfaceID(a.ID), Context: map[string]any{"description": "Tea", "amount": "1"}}})
	if len(reply.Rows) != sampleRowCount+1 {
		t.Fatalf("preview rows = %+v", reply.Rows)
	}
	// The real routes refuse: not published.
	rec := w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"),
		action(a, "add_expense", map[string]any{"description": "Real", "amount": "1"}))
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_not_published" {
		t.Fatalf("a real action on a draft = %s", rec.Body)
	}
	expect(t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil), http.StatusConflict)
	if after := everythingStored(t, w.e); after != before {
		t.Fatalf("a draft changed what is stored:\n%s\n%s", before, after)
	}
}

// What only the draft declares is the preview's: an action the live release doesn't have can't be
// run for real, whatever the preview says.
func TestADraftsOwnActionsNeverRunLive(t *testing.T) {
	w := newWorld(t)
	s, a := w.alicePrivate, w.aliceApp
	cur := w.view(w.alice, s, a)
	draft := *cur.Draft
	draft.Actions = map[string]Action{}
	for name, act := range cur.Draft.Actions {
		draft.Actions[name] = act
	}
	draft.Actions["add_again"] = Action{Kind: ActionInsert}
	w.mustPatchDraft(w.alice, s, cur, draft)
	before := everythingStored(t, w.e)

	// The draft's preview runs it, on sample rows.
	rows := w.mustPreview(w.alice, s, a, PreviewRequest{}).Rows
	reply := w.mustPreview(w.alice, s, a, PreviewRequest{Rows: rows,
		Action: &ClientAction{Name: "add_again", SurfaceID: SurfaceID(a.ID), Context: map[string]any{"description": "Tea", "amount": "1"}}})
	if len(reply.Rows) != len(rows)+1 {
		t.Fatalf("preview rows = %+v", reply.Rows)
	}
	// The live app doesn't know it, and nothing is stored.
	rec := w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"),
		action(a, "add_again", map[string]any{"description": "Real", "amount": "1"}))
	expect(t, rec, http.StatusUnprocessableEntity)
	if got := w.rowsOf(w.alice, s, a); len(got) != 1 {
		t.Fatalf("live rows = %v", got)
	}
	if after := everythingStored(t, w.e); after != before {
		t.Fatalf("the preview changed what is stored")
	}
}

// What a client sends back is checked: too many rows, two with the same id, rows the draft
// refuses.
func TestPreviewChecksTheRowsItIsGiven(t *testing.T) {
	w := newWorld(t)
	s, a := w.alicePrivate, w.aliceApp
	ok := map[string]any{"description": "x", "amount": float64(1)}
	var many []PreviewRow
	for i := 1; i <= previewMaxRows+1; i++ {
		many = append(many, PreviewRow{ID: int64(i), Revision: 1, Data: ok})
	}
	rec := w.preview(w.alice, s, a, PreviewRequest{Rows: many})
	expect(t, rec, http.StatusConflict)
	if errorCode(t, rec) != "app_full" {
		t.Fatalf("too many rows = %s", rec.Body)
	}
	for name, rows := range map[string][]PreviewRow{
		"the same id twice":  {{ID: 1, Revision: 1, Data: ok}, {ID: 1, Revision: 1, Data: ok}},
		"no id":              {{Revision: 1, Data: ok}},
		"no revision":        {{ID: 1, Data: ok}},
		"a required field":   {{ID: 1, Revision: 1, Data: map[string]any{"amount": float64(1)}}},
		"a value that fails": {{ID: 1, Revision: 1, Data: map[string]any{"description": "x", "amount": "text"}}},
	} {
		rec := w.preview(w.alice, s, a, PreviewRequest{Rows: rows})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, body %s", name, rec.Code, rec.Body)
		}
	}
	// An empty list is a preview whose rows were all deleted, not a new start.
	reply := w.mustPreview(w.alice, s, a, PreviewRequest{Rows: []PreviewRow{}})
	if len(reply.Rows) != 0 {
		t.Fatalf("an empty preview has rows: %+v", reply.Rows)
	}
	// An app that is offline has no preview.
	w.mustSetOffline(w.alice, s, a)
	expect(t, w.preview(w.alice, s, a, PreviewRequest{}), http.StatusConflict)
}

func (w *world) mustSetOffline(u *user, s access.Space, a App) {
	w.e.t.Helper()
	cur := w.view(u, s, a)
	expect(w.e.t, u.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": cur.Version, "archived": true}), http.StatusOK)
}

// Sample rows fit every bundled app's collection, whatever its fields: a preview starts with
// rows the live app would accept.
func TestSampleRowsFitTheirCollections(t *testing.T) {
	all, err := Examples()
	if err != nil {
		t.Fatal(err)
	}
	for _, ex := range all {
		rows := SampleRows(ex.Definition.Collection)
		if len(rows) != sampleRowCount {
			t.Errorf("%s: %d sample rows", ex.Name, len(rows))
		}
		for _, r := range rows {
			if err := ex.Definition.Collection.Fits(r.Data); err != nil {
				t.Errorf("%s: %v", ex.Name, err)
			}
		}
	}
	// Limits are kept: a short text, a number between its bounds, a choice's option.
	lo, hi := 15.0, 25.0
	rows := SampleRows(Collection{Fields: []Field{
		{Name: "title", Type: TypeString, MaxLength: 4},
		{Name: "qty", Type: TypeNumber, Min: &lo, Max: &hi},
		{Name: "kind", Type: TypeChoice, Options: []string{"a", "b"}},
	}})
	if len(rows) != sampleRowCount || rows[0].Data["title"] != "Titl" || rows[2].Data["qty"] != 25.0 || rows[2].Data["kind"] != "a" {
		t.Fatalf("limited sample rows = %+v", rows)
	}
}
