package apps

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
)

func (w *world) editable(u *user, s access.Space) App {
	w.e.t.Helper()
	def := example(w.e.t)
	def.Actions["edit_expense"] = Action{Kind: ActionUpdate}
	def.Summaries["n"] = Summary{Kind: "count"}
	rec := u.do(http.MethodPost, appsPath(s, ""), map[string]any{"name": "Editable", "definition": def})
	expect(w.e.t, rec, http.StatusCreated)
	a := w.publish(u, s, decode[App](w.e.t, rec))
	if _, err := w.e.db.Exec(`UPDATE apps SET version = 1 WHERE id = ?`, a.ID); err != nil {
		w.e.t.Fatal(err)
	}
	a.Version = 1
	return a
}

func (w *world) act(u *user, s access.Space, a App, name string, ctx map[string]any, revision *int64) *httptest.ResponseRecorder {
	w.e.t.Helper()
	act := action(a, name, ctx)
	act.ExpectedRevision = revision
	return u.do(http.MethodPost, appPath(s, a.ID, "/actions"), act)
}

func rev(n int64) *int64 { return &n }

func (w *world) idOf(u *user, s access.Space, a App, d string) (id, revision int64) {
	w.e.t.Helper()
	for _, it := range w.items(u, s, a) {
		if it["description"] == d {
			return int64(it["id"].(float64)), int64(it["_revision"].(float64))
		}
	}
	w.e.t.Fatalf("no record %q is read", d)
	return 0, 0
}

type conflictReply struct {
	Code    string
	Params  map[string]any
	Current map[string]any
}

func (w *world) history(u *user, s access.Space, a App, id int64) *httptest.ResponseRecorder {
	return u.do(http.MethodGet, appPath(s, a.ID, fmt.Sprintf("/records/%d/history", id)), nil)
}

func (w *world) deleted(u *user, s access.Space, a App) *httptest.ResponseRecorder {
	return u.do(http.MethodGet, appPath(s, a.ID, "/records/deleted"), nil)
}

func (w *world) records(u *user, s access.Space, a App) *httptest.ResponseRecorder {
	return u.do(http.MethodGet, appPath(s, a.ID, "/records"), nil)
}

func (w *world) restore(u *user, s access.Space, a App, id int64, revision *int64) *httptest.ResponseRecorder {
	return u.do(http.MethodPost, appPath(s, a.ID, fmt.Sprintf("/records/%d/restore", id)), map[string]any{"expected_revision": revision})
}

func operations(h History) string {
	var ops []string
	for _, v := range h.History {
		ops = append(ops, fmt.Sprintf("%d:%s", v.Revision, v.Operation))
	}
	return strings.Join(ops, " ")
}

func TestStaleEditDoesNotOverwriteNewerData(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	id, r1 := w.idOf(w.alice, s, a, "lunch")
	if r1 != 1 {
		t.Fatalf("a new record's revision = %d, want 1", r1)
	}

	expect(t, w.act(w.owner, s, a, "edit_expense", map[string]any{"id": id, "amount": "20"}, rev(1)), http.StatusOK)
	rec := w.act(w.alice, s, a, "edit_expense", map[string]any{"id": id, "amount": "30"}, rev(1))
	expect(t, rec, http.StatusConflict)
	reply := decode[conflictReply](t, rec)
	if reply.Code != "app_record_conflict" || reply.Current["amount"] != 20.0 || reply.Current["_revision"] != 2.0 || reply.Params["id"] != float64(id) {
		t.Fatalf("409 reply = %s", rec.Body)
	}
	if _, got := w.idOf(w.alice, s, a, "lunch"); got != 2 {
		t.Fatalf("revision after a refused edit = %d, want 2", got)
	}
	if items := w.items(w.alice, s, a); items[0]["amount"] != 20.0 {
		t.Fatalf("the stale edit overwrote the newer data: %v", items[0])
	}
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": id}, rev(1)), http.StatusConflict)
	if len(w.items(w.alice, s, a)) != 1 {
		t.Fatal("a stale delete removed the record")
	}
	expect(t, w.act(w.alice, s, a, "edit_expense", map[string]any{"id": id, "amount": "30"}, rev(2)), http.StatusOK)
	if _, got := w.idOf(w.alice, s, a, "lunch"); got != 3 {
		t.Fatalf("revision = %d, want 3", got)
	}
	expect(t, w.act(w.alice, s, a, "edit_expense", map[string]any{"id": id, "amount": "40"}, nil), http.StatusOK)
	expect(t, w.act(w.alice, s, a, "edit_expense", map[string]any{"id": id, "amount": "40"}, rev(4)), http.StatusOK)
	if _, got := w.idOf(w.alice, s, a, "lunch"); got != 4 {
		t.Fatalf("revision after a change that changed nothing = %d, want 4", got)
	}
	expect(t, w.act(w.alice, s, a, "edit_expense", map[string]any{"id": id, "amount": "1"}, rev(0)), http.StatusUnprocessableEntity)
	expect(t, w.act(w.alice, s, a, "add_expense", map[string]any{"description": "x", "amount": "1"}, rev(1)), http.StatusUnprocessableEntity)
}

func TestConflictShowsNothingTheCallerCantRead(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "alice-row", "10")
	id, _ := w.idOf(w.alice, s, a, "alice-row")
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppUser), grant(w.alice.id, access.AppRoles...))
	rec := w.act(w.owner, s, a, "edit_expense", map[string]any{"id": id, "amount": "1"}, rev(7))
	expect(t, rec, http.StatusUnprocessableEntity)
	if strings.Contains(rec.Body.String(), "alice-row") || strings.Contains(rec.Body.String(), "current") {
		t.Fatalf("a refusal to someone who can't read the record: %s", rec.Body)
	}
	rec = w.act(w.alice, s, a, "edit_expense", map[string]any{"id": id, "amount": "1"}, rev(7))
	expect(t, rec, http.StatusConflict)
	if decode[conflictReply](t, rec).Current["description"] != "alice-row" {
		t.Fatalf("409 reply = %s", rec.Body)
	}
}

func TestDeleteAndRestoreKeepHistory(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	w.add(w.alice, s, a, "coffee", "5")
	id, _ := w.idOf(w.alice, s, a, "lunch")
	expect(t, w.act(w.owner, s, a, "edit_expense", map[string]any{"id": id, "amount": "12"}, rev(1)), http.StatusOK)
	rec := w.act(w.alice, s, a, "delete_expense", map[string]any{"id": id}, rev(2))
	expect(t, rec, http.StatusOK)
	if body := rec.Body.String(); strings.Contains(body, "lunch") || !strings.Contains(body, `"total":"5.00"`) || !strings.Contains(body, `"n":"1"`) {
		t.Fatalf("after the delete: %s", body)
	}
	if rows := w.rowsOf(w.alice, s, a); len(rows) != 1 || rows[0] != "coffee" {
		t.Fatalf("rows = %v", rows)
	}
	if body := w.alice.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil).Body.String(); !strings.Contains(body, `"total":"5.00"`) || strings.Contains(body, "lunch") {
		t.Fatalf("surface: %s", body)
	}
	if got := w.view(w.alice, s, a).Rows; got != 1 {
		t.Fatalf("rows in the app = %d, want 1", got)
	}
	list := decode[struct{ Apps []App }](t, w.alice.do(http.MethodGet, appsPath(s, ""), nil)).Apps
	for _, x := range list {
		if x.ID == a.ID && x.Rows != 1 {
			t.Fatalf("rows in the list = %d, want 1", x.Rows)
		}
	}
	tools, err := w.e.s.ListRows(context.Background(), Actor{UserID: w.alice.id, Agent: true}, s.ID, a.ID)
	if err != nil || len(tools) != 1 {
		t.Fatalf("the agent reads %v, %v", tools, err)
	}
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": id}, nil), http.StatusUnprocessableEntity)
	expect(t, w.act(w.alice, s, a, "edit_expense", map[string]any{"id": id, "amount": "1"}, nil), http.StatusUnprocessableEntity)

	live := decode[Records](t, w.records(w.alice, s, a))
	if len(live.Records) != 1 || live.Records[0].Data["description"] != "coffee" || live.Records[0].Revision != 1 ||
		live.Records[0].CreatedBy == nil || live.Records[0].CreatedBy.ID != w.alice.id || live.Records[0].DeletedAt != nil {
		t.Fatalf("records = %+v", live)
	}
	del := decode[Records](t, w.deleted(w.alice, s, a))
	if len(del.Records) != 1 || del.Records[0].ID != id || del.Records[0].Revision != 3 || del.Records[0].Data["description"] != "lunch" ||
		del.Records[0].DeletedAt == nil || del.Records[0].DeletedBy == nil || del.Records[0].DeletedBy.ID != w.alice.id || del.Records[0].DeletedAction != "delete_expense" {
		t.Fatalf("deleted = %+v", del)
	}
	h := decode[History](t, w.history(w.alice, s, a, id))
	if got := operations(h); got != "3:delete 2:update 1:insert" {
		t.Fatalf("history = %s", got)
	}
	if h.History[1].By == nil || h.History[1].By.ID != w.owner.id || h.History[1].Action != "edit_expense" || h.History[1].Data["amount"] != 12.0 ||
		h.History[2].Data["amount"] != 10.0 || h.History[2].Action != "add_expense" || h.History[2].By.ID != w.alice.id {
		t.Fatalf("history = %+v", h.History)
	}
	if h.Record.DeletedAt == nil {
		t.Fatalf("the record of a deleted one isn't marked deleted: %+v", h.Record)
	}

	rec = w.restore(w.alice, s, a, id, rev(2))
	expect(t, rec, http.StatusConflict)
	if decode[conflictReply](t, rec).Code != "app_record_conflict" {
		t.Fatalf("409 reply = %s", rec.Body)
	}
	rec = w.restore(w.alice, s, a, id, rev(3))
	expect(t, rec, http.StatusOK)
	if got := decode[Record](t, rec); got.Revision != 4 || got.DeletedAt != nil || got.Data["amount"] != 12.0 || got.UpdatedBy.ID != w.alice.id {
		t.Fatalf("restored = %+v", got)
	}
	if rows := w.rowsOf(w.alice, s, a); len(rows) != 2 {
		t.Fatalf("rows after the restore = %v", rows)
	}
	if body := w.alice.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil).Body.String(); !strings.Contains(body, `"total":"17.00"`) || !strings.Contains(body, `"n":"2"`) {
		t.Fatalf("surface after the restore: %s", body)
	}
	if got := len(decode[Records](t, w.deleted(w.alice, s, a)).Records); got != 0 {
		t.Fatalf("%d deleted records after the restore", got)
	}
	h = decode[History](t, w.history(w.alice, s, a, id))
	if got := operations(h); got != "4:restore 3:delete 2:update 1:insert" {
		t.Fatalf("history after the restore = %s", got)
	}
	rec = w.restore(w.alice, s, a, id, nil)
	expect(t, rec, http.StatusConflict)
	if decode[conflictReply](t, rec).Current["_revision"] != 4.0 {
		t.Fatalf("409 reply = %s", rec.Body)
	}
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": id}, rev(4)), http.StatusOK)
	expect(t, w.restore(w.alice, s, a, id, nil), http.StatusOK)
	if _, got := w.idOf(w.alice, s, a, "lunch"); got != 6 {
		t.Fatalf("revision = %d, want 6", got)
	}
}

func TestRecordProvenance(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	agent := Actor{UserID: w.alice.id, Agent: true, Via: "my-token"}
	if _, err := w.e.s.act(context.Background(), s, agent, a.ID, action(a, "add_expense", map[string]any{"description": "by-agent", "amount": "1"})); err != nil {
		t.Fatal(err)
	}
	id, _ := w.idOf(w.alice, s, a, "by-agent")
	h := decode[History](t, w.history(w.alice, s, a, id))
	by := h.Record.CreatedBy
	if by == nil || by.ID != w.alice.id || !by.Agent || by.Via != "my-token" || h.Record.CreatedAction != "add_expense" || h.Record.UpdatedAction != "add_expense" {
		t.Fatalf("record = %+v", h.Record)
	}
	if v := h.History[0]; v.Operation != "insert" || v.By == nil || !v.By.Agent || v.By.Via != "my-token" || v.Action != "add_expense" || v.Synthetic {
		t.Fatalf("version = %+v", v)
	}
}

func TestRowLimitsCountLiveRowsOnly(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "first", "1")
	first, _ := w.idOf(w.alice, s, a, "first")
	tx, err := w.e.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < MaxRows; i++ {
		if _, err := tx.Exec(`INSERT INTO app_rows (app_id, data, created_by) VALUES (?, '{"description":"x","amount":1}', ?)`, a.ID, w.alice.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	full := func() *httptest.ResponseRecorder {
		return w.act(w.alice, s, a, "add_expense", map[string]any{"description": "over", "amount": "1"}, nil)
	}
	expect(t, full(), http.StatusConflict)
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": first}, nil), http.StatusOK)
	expect(t, full(), http.StatusOK)
	rec := w.restore(w.alice, s, a, first, nil)
	expect(t, rec, http.StatusConflict)
	if decode[conflictReply](t, rec).Code != "app_full" {
		t.Fatalf("409 reply = %s", rec.Body)
	}
	if got := w.view(w.alice, s, a).Rows; got != MaxRows {
		t.Fatalf("rows = %d, want %d", got, MaxRows)
	}
	var tombstones int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM app_rows WHERE app_id = ? AND deleted_at IS NOT NULL`, a.ID).Scan(&tombstones); err != nil || tombstones != 1 {
		t.Fatalf("tombstones = %d, %v", tombstones, err)
	}
}

func TestRestoreNeedsTheRecordToFitTheCollection(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	id, _ := w.idOf(w.alice, s, a, "lunch")
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": id}, nil), http.StatusOK)
	def := *w.view(w.alice, s, a).Draft
	patched := w.mustPatchDraft(w.alice, s, a, with(def, Field{Name: "note", Type: TypeString, Required: true}))
	w.publish(w.alice, s, patched)
	rec := w.restore(w.alice, s, a, id, nil)
	expect(t, rec, http.StatusBadRequest)
	if decode[conflictReply](t, rec).Code != "app_field_required" {
		t.Fatalf("reply = %s", rec.Body)
	}
	if got := len(decode[Records](t, w.deleted(w.alice, s, a)).Records); got != 1 {
		t.Fatalf("deleted = %d", got)
	}
}

func TestOfflineAppRestoresNothing(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	id, _ := w.idOf(w.alice, s, a, "lunch")
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": id}, nil), http.StatusOK)
	expect(t, w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": 1, "archived": true}), http.StatusOK)
	expect(t, w.history(w.alice, s, a, id), http.StatusOK)
	expect(t, w.deleted(w.alice, s, a), http.StatusOK)
	expect(t, w.restore(w.alice, s, a, id, nil), http.StatusConflict)
}

func TestHistoryDeletedAndRestoreFollowAccess(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "alice-row", "1")
	w.add(w.owner, s, a, "owner-row", "2")
	aliceRow, _ := w.idOf(w.alice, s, a, "alice-row")
	ownerRow, _ := w.idOf(w.alice, s, a, "owner-row")
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppUser), grant(w.alice.id, access.AppRoles...), grant(w.bob.id, access.AppReader))
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": aliceRow}, nil), http.StatusOK)
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": ownerRow}, nil), http.StatusOK)

	del := decode[Records](t, w.deleted(w.owner, s, a))
	if len(del.Records) != 1 || del.Records[0].ID != ownerRow {
		t.Fatalf("the user's deleted records = %+v", del)
	}
	if strings.Contains(w.deleted(w.owner, s, a).Body.String(), "alice-row") {
		t.Fatal("the list of deleted records shows another's")
	}
	expect(t, w.history(w.owner, s, a, ownerRow), http.StatusOK)
	if live := decode[Records](t, w.records(w.owner, s, a)); len(live.Records) != 0 {
		t.Fatalf("the user lists %+v live records, all deleted", live)
	}
	rec := w.history(w.owner, s, a, aliceRow)
	expect(t, rec, http.StatusNotFound)
	missing := w.history(w.owner, s, a, 99999)
	if rec.Body.String() != missing.Body.String() {
		t.Fatalf("another's record and one that doesn't exist are answered differently: %s vs %s", rec.Body, missing.Body)
	}
	expect(t, w.restore(w.owner, s, a, aliceRow, nil), http.StatusNotFound)
	if strings.Contains(w.deleted(w.alice, s, a).Body.String(), "owner-row") == false {
		t.Fatal("a reader of all doesn't see the deleted records")
	}
	expect(t, w.restore(w.owner, s, a, ownerRow, nil), http.StatusOK)
	if rows := w.rowsOf(w.owner, s, a); len(rows) != 1 || rows[0] != "owner-row" {
		t.Fatalf("the user reads %v", rows)
	}
	if live := decode[Records](t, w.records(w.owner, s, a)); len(live.Records) != 1 || live.Records[0].ID != ownerRow || live.Records[0].Revision != 3 {
		t.Fatalf("the user lists %+v", live)
	}
	if rows := w.rowsOf(w.alice, s, a); len(rows) != 1 {
		t.Fatalf("alice reads %v", rows)
	}

	expect(t, w.history(w.bob, s, a, aliceRow), http.StatusOK)
	if got := len(decode[Records](t, w.deleted(w.bob, s, a)).Records); got != 1 {
		t.Fatalf("a reader sees %d deleted records", got)
	}
	expect(t, w.restore(w.bob, s, a, aliceRow, nil), http.StatusForbidden)

	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppBuilder), grant(w.alice.id, access.AppRoles...))
	expectForbidden(t, w.history(w.owner, s, a, aliceRow), "read_records")
	expectForbidden(t, w.deleted(w.owner, s, a), "read_records")
	expectForbidden(t, w.restore(w.owner, s, a, aliceRow, nil), "modify_records")
	expectForbidden(t, w.history(w.owner, s, a, 99999), "read_records")
}

func TestRecordRoutesAnswerNotFoundWithoutAccess(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	id, _ := w.idOf(w.alice, s, a, "lunch")
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": id}, nil), http.StatusOK)

	all := func(u *user, s access.Space, a App) []*httptest.ResponseRecorder {
		return []*httptest.ResponseRecorder{w.history(u, s, a, id), w.deleted(u, s, a), w.restore(u, s, a, id, nil), w.records(u, s, a)}
	}
	check := func(name string, recs []*httptest.ResponseRecorder, want int) {
		t.Helper()
		for i, rec := range recs {
			if rec.Code != want || strings.Contains(rec.Body.String(), "lunch") {
				t.Errorf("%s, route %d: status %d, body %s; want %d and nothing of the record", name, i, rec.Code, rec.Body, want)
			}
		}
	}
	check("mallory", all(w.mallory, s, a), http.StatusNotFound)
	check("stranger", all(w.stranger, s, a), http.StatusNotFound)
	check("stranger, own workspace", all(w.stranger, in(w.other, s), a), http.StatusNotFound)
	check("alice, wrong space", all(w.alice, w.alicePrivate, a), http.StatusNotFound)
	check("mallory, private space", all(w.mallory, w.alicePrivate, w.aliceApp), http.StatusNotFound)
	w.mustSetRoles(w.alice, s, a, grant(w.alice.id, access.AppRoles...))
	bob := all(w.bob, s, a)
	check("bob, unnamed in a restricted app", bob[:2], http.StatusNotFound)
	check("bob, restoring", bob[2:3], http.StatusForbidden)
	check("bob, listing", bob[3:], http.StatusNotFound)
	expect(t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/records/abc/history"), nil), http.StatusNotFound)
	anon := &user{e: w.e}
	for _, rec := range all(anon, s, a) {
		expect(t, rec, http.StatusUnauthorized)
	}
}
