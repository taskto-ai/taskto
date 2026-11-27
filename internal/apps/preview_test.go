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

func TestPreviewStartsOnSampleRowsNotRealOnes(t *testing.T) {
	w := newWorld(t)
	s, a := w.alicePrivate, w.aliceApp
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
	if len(reply.Messages) != 3 || reply.Messages[0]["createSurface"] == nil {
		t.Fatalf("messages = %v", reply.Messages)
	}
	again := w.mustPreview(w.alice, s, a, PreviewRequest{})
	if a, b := fmt.Sprint(reply.Rows), fmt.Sprint(again.Rows); a != b {
		t.Fatalf("sample rows differ between starts: %s, %s", a, b)
	}
}

func TestPreviewActionsWriteNothing(t *testing.T) {
	w := newWorld(t)
	s, a := w.alicePrivate, w.aliceApp
	before := everythingStored(t, w.e)

	rows := w.mustPreview(w.alice, s, a, PreviewRequest{}).Rows

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
	rec := w.preview(w.alice, s, a, PreviewRequest{Rows: rows,
		Action: &ClientAction{Name: "add_expense", SurfaceID: SurfaceID(a.ID), Context: map[string]any{"amount": "3"}}})
	expect(t, rec, http.StatusUnprocessableEntity)
	if errorCode(t, rec) != "app_field_required" {
		t.Fatalf("a row without its description = %s", rec.Body)
	}

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
	opened := do(start.Rows, "open_task", map[string]any{"id": float64(2)})
	if !strings.Contains(fmt.Sprint(opened.Messages), "/record") {
		t.Fatalf("open: %v", opened.Messages)
	}
	done := do(start.Rows, "complete_task", map[string]any{"id": float64(2), "done": true})
	if done.Rows[1].Revision != 2 || done.Rows[1].Data["done"] != true {
		t.Fatalf("update: %+v", done.Rows)
	}
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

func TestPreviewIsForBuildersAndPublishers(t *testing.T) {
	w := newWorld(t)
	s, a := w.shared, w.sharedApp
	expectForbidden(t, w.preview(w.bob, s, a, PreviewRequest{}), "design")
	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppBuilder), grant(w.owner.id, access.AppUser))
	expectForbidden(t, w.preview(w.owner, s, a, PreviewRequest{}), "design")
	expect(t, w.preview(w.alice, s, a, PreviewRequest{}), http.StatusOK)
	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppBuilder), grant(w.owner.id, access.AppPublisher))
	expect(t, w.preview(w.owner, s, a, PreviewRequest{}), http.StatusOK)

	expect(t, w.preview(w.mallory, s, a, PreviewRequest{}), http.StatusNotFound)
	expect(t, w.preview(w.stranger, s, a, PreviewRequest{}), http.StatusNotFound)
	expect(t, w.preview(w.stranger, in(w.other, s), a, PreviewRequest{}), http.StatusNotFound)
	expect(t, w.preview(w.bob, w.alicePrivate, w.aliceApp, PreviewRequest{}), http.StatusNotFound)
	expect(t, w.preview(w.alice, w.alicePrivate, w.sharedApp, PreviewRequest{}), http.StatusNotFound)
	expect(t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/preview"), nil), http.StatusMethodNotAllowed)
}

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

	rows := w.mustPreview(w.alice, s, a, PreviewRequest{}).Rows
	reply := w.mustPreview(w.alice, s, a, PreviewRequest{Rows: rows,
		Action: &ClientAction{Name: "add_again", SurfaceID: SurfaceID(a.ID), Context: map[string]any{"description": "Tea", "amount": "1"}}})
	if len(reply.Rows) != len(rows)+1 {
		t.Fatalf("preview rows = %+v", reply.Rows)
	}
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
	if errorCode(t, rec) != "app_preview_full" {
		t.Fatalf("too many rows = %s", rec.Body)
	}
	if got := decode[struct{ Params map[string]any }](t, rec).Params["max_rows"]; got != float64(previewMaxRows) {
		t.Fatalf("too many rows names the limit %v, want %d", got, previewMaxRows)
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
	reply := w.mustPreview(w.alice, s, a, PreviewRequest{Rows: []PreviewRow{}})
	if len(reply.Rows) != 0 {
		t.Fatalf("an empty preview has rows: %+v", reply.Rows)
	}
	w.mustSetOffline(w.alice, s, a)
	expect(t, w.preview(w.alice, s, a, PreviewRequest{}), http.StatusConflict)
}

func (w *world) mustSetOffline(u *user, s access.Space, a App) {
	w.e.t.Helper()
	cur := w.view(u, s, a)
	expect(w.e.t, u.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": cur.Version, "archived": true}), http.StatusOK)
}

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
			if err := ex.Definition.Collection.forPreview().Fits(r.Data); err != nil {
				t.Errorf("%s: %v", ex.Name, err)
			}
		}
	}
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

func TestPreviewTakesAsManyValidRowsAsItHolds(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Notes")
	big := strings.Repeat("x", DefaultMaxLength)
	var rows []PreviewRow
	for i := 1; i <= previewMaxRows; i++ {
		rows = append(rows, PreviewRow{ID: int64(i), Revision: 1, Data: map[string]any{"title": big}})
	}
	if raw, _ := json.Marshal(PreviewRequest{Rows: rows}); len(raw) <= maxBody {
		t.Fatalf("the test's request is %d bytes: it must be over the definition body limit %d", len(raw), maxBody)
	}
	reply := w.mustPreview(w.alice, s, a, PreviewRequest{Rows: rows})
	if len(reply.Rows) != previewMaxRows {
		t.Fatalf("%d rows came back, want %d", len(reply.Rows), previewMaxRows)
	}
}

func TestPreviewRefusesAnOversizeRequest(t *testing.T) {
	w := newWorld(t)
	s, a := w.alicePrivate, w.aliceApp
	body := `{"rows": [], "pad": "` + strings.Repeat("x", previewMaxBody) + `"}`
	r := httptest.NewRequest(http.MethodPost, appPath(s, a.ID, "/preview"), strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.AddCookie(w.alice.cookie)
	rec := httptest.NewRecorder()
	w.e.h.ServeHTTP(rec, r)
	expectCode(t, rec, http.StatusRequestEntityTooLarge, "app_preview_too_large")
}

func TestPreviewNeverReferencesAttachments(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	const id = "0123456789abcdef0123456789abcdef"
	start := w.mustPreview(w.alice, s, a, PreviewRequest{})
	for _, r := range start.Rows {
		if _, has := r.Data["photos"]; has {
			t.Fatalf("a sample row has photos: %+v", r)
		}
		if _, has := r.Data["receipt"]; has {
			t.Fatalf("a sample row has a receipt: %+v", r)
		}
	}
	given := []PreviewRow{{ID: 1, Revision: 1, Data: map[string]any{"title": "x", "photos": []any{id}}}}
	reply := w.mustPreview(w.alice, s, a, PreviewRequest{Rows: given})
	action := &ClientAction{Name: "add_note", SurfaceID: SurfaceID(a.ID), Context: map[string]any{"title": "y", "photos": []any{id}, "receipt": []any{id}}}
	reply = w.mustPreview(w.alice, s, a, PreviewRequest{Rows: reply.Rows, Action: action})
	if len(reply.Rows) != 2 {
		t.Fatalf("rows = %+v", reply.Rows)
	}
	if out, _ := jsonString(reply); strings.Contains(out, id) {
		t.Fatalf("the preview returned an attachment id: %s", out)
	}
}

func TestPreviewHandlesCaptureFields(t *testing.T) {
	c := Collection{Fields: []Field{
		{Name: "place", Type: TypeLocation, Required: true},
		{Name: "ean", Type: TypeBarcode, Format: "ean_13", MaxLength: 20, Required: true},
		{Name: "ean8", Type: TypeBarcode, Format: "ean_8", MaxLength: 20},
		{Name: "upc", Type: TypeBarcode, Format: "upc_a", MaxLength: 20},
		{Name: "upce", Type: TypeBarcode, Format: "upc_e", MaxLength: 20},
		{Name: "qr", Type: TypeBarcode, Format: "qr_code", MaxLength: 6},
		{Name: "signed", Type: TypeSignature, Required: true},
		{Name: "photos", Type: TypeAttachment, Required: true},
	}}
	rows := SampleRows(c)
	if len(rows) != sampleRowCount {
		t.Fatalf("%d sample rows: a required capture field left them out", len(rows))
	}
	for _, r := range rows {
		for _, f := range c.Fields {
			v, has := r.Data[f.Name]
			switch f.Type {
			case TypeSignature, TypeAttachment:
				if has {
					t.Errorf("a sample row holds a picture: %s = %v", f.Name, v)
				}
			default:
				if _, present, err := f.parse(v); err != nil || !present {
					t.Errorf("%s = %v: %v", f.Name, v, err)
				}
			}
		}
	}
	if rows[0].Data["ean"] == rows[1].Data["ean"] || !jsonEqual(SampleRows(c), rows) {
		t.Errorf("sample codes: %v, %v", rows[0].Data["ean"], rows[1].Data["ean"])
	}
}

func TestPreviewNeverReferencesASignature(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createCapture(w.alice, s)
	const id = "0123456789abcdef0123456789abcdef"
	start := w.mustPreview(w.alice, s, a, PreviewRequest{})
	for _, r := range start.Rows {
		if _, has := r.Data["signed"]; has {
			t.Fatalf("a sample row has a signature: %+v", r)
		}
		if _, has := r.Data["place"]; !has {
			t.Fatalf("a sample row has no place: %+v", r)
		}
	}
	given := []PreviewRow{{ID: 1, Revision: 1, Data: map[string]any{"title": "x", "signed": id}}}
	reply := w.mustPreview(w.alice, s, a, PreviewRequest{Rows: given})
	action := &ClientAction{Name: "add_note", SurfaceID: SurfaceID(a.ID), Context: map[string]any{"title": "y", "signed": id}}
	reply = w.mustPreview(w.alice, s, a, PreviewRequest{Rows: reply.Rows, Action: action})
	if len(reply.Rows) != 2 {
		t.Fatalf("rows = %+v", reply.Rows)
	}
	if out, _ := jsonString(reply); strings.Contains(out, id) {
		t.Fatalf("the preview returned a signature's id: %s", out)
	}
}
