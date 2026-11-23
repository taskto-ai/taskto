package apps

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
)

// What a device does offline when it edits or deletes a downloaded record (docs/offline.md): it
// sends the action later with the record's revision it was based on and a stable operation key.
// These tests are the server's side of that: two devices of one person (or two people) editing
// one record never overwrite each other, a deleted record is never brought back by an edit, and
// a send made again is made once.

// replay presses an action the way a replaying device does: with the revision it was based on and
// its operation key.
func (w *world) replay(u *user, s access.Space, a App, operation, name string, ctx map[string]any, revision *int64) *httptest.ResponseRecorder {
	w.e.t.Helper()
	act := action(a, name, ctx)
	act.ExpectedRevision = revision
	act.Operation = operation
	return u.do(http.MethodPost, appPath(s, a.ID, "/actions"), act)
}

func (w *world) revisionOf(u *user, s access.Space, a App, id int64) (revision int64, amount any) {
	w.e.t.Helper()
	for _, it := range w.items(u, s, a) {
		if int64(it["id"].(float64)) == id {
			return int64(it["_revision"].(float64)), it["amount"]
		}
	}
	w.e.t.Fatalf("record %d is not read", id)
	return 0, nil
}

func TestDownloadSaysWhichRecordsTheCallerMayChange(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "alice-row", "1")
	w.add(w.owner, s, a, "owner-row", "2")
	aliceRow, _ := w.idOf(w.alice, s, a, "alice-row")
	ownerRow, _ := w.idOf(w.alice, s, a, "owner-row")
	// Members who may change every record are told so of all of them.
	got := decode[OfflineApp](t, w.download(w.alice, s, a))
	if !slices.Equal(got.Modifiable, []int64{aliceRow, ownerRow}) {
		t.Fatalf("modifiable = %v, want both rows", got.Modifiable)
	}
	// Someone whose role changes only their own records is told only of theirs, and one who may
	// change none, of none: a client offers no offline edit for the rest.
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppUser), grant(w.bob.id, access.AppReader), grant(w.alice.id, access.AppRoles...))
	own := decode[OfflineApp](t, w.download(w.owner, s, a))
	if own.Scope != "own" || !slices.Equal(own.Modifiable, []int64{ownerRow}) {
		t.Fatalf("a user's download = scope %s, modifiable %v", own.Scope, own.Modifiable)
	}
	reader := decode[OfflineApp](t, w.download(w.bob, s, a))
	if reader.Modifiable == nil || len(reader.Modifiable) != 0 {
		t.Fatalf("a reader's modifiable = %#v, want an empty list", reader.Modifiable)
	}
	if !strings.Contains(w.download(w.bob, s, a).Body.String(), `"modifiable":[]`) {
		t.Fatal("modifiable is not sent as a list when empty")
	}
}

// Two devices edit one record from the same revision: the second is a conflict that changes
// nothing and shows the record as it is, and applying it on top of that revision works, with the
// first device's change kept.
func TestTwoDevicesEditingOneRecordConflictAndNothingIsOverwritten(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	id, base := w.idOf(w.alice, s, a, "lunch")
	// Both devices downloaded revision `base`, and were offline.
	expect(t, w.replay(w.alice, s, a, "device-a-edit", "edit_expense", map[string]any{"id": id, "description": "late lunch"}, rev(base)), http.StatusOK)
	rec := w.replay(w.alice, s, a, "device-b-edit", "edit_expense", map[string]any{"id": id, "amount": "99"}, rev(base))
	expect(t, rec, http.StatusConflict)
	reply := decode[conflictReply](t, rec)
	if reply.Code != "app_record_conflict" || reply.Current["description"] != "late lunch" || reply.Current["_revision"] != float64(base+1) {
		t.Fatalf("the conflict does not show the record as it is: %s", rec.Body)
	}
	if revision, amount := w.revisionOf(w.alice, s, a, id); revision != base+1 || amount != 10.0 {
		t.Fatalf("the refused edit changed the record: revision %d, amount %v", revision, amount)
	}
	// The refused request left no receipt: the same key can be sent again, now on top of the
	// current revision, and it is made once however often it is sent.
	for i := 0; i < 2; i++ {
		expect(t, w.replay(w.alice, s, a, "device-b-reapplied", "edit_expense", map[string]any{"id": id, "amount": "99"}, rev(base+1)), http.StatusOK)
	}
	if revision, amount := w.revisionOf(w.alice, s, a, id); revision != base+2 || amount != 99.0 {
		t.Fatalf("after the reapplied edit: revision %d, amount %v", revision, amount)
	}
	if items := w.items(w.alice, s, a); items[0]["description"] != "late lunch" {
		t.Fatalf("the first device's change was lost: %v", items[0])
	}
	// A device whose answer was lost sends its edit again after others have moved on: still one
	// change, answered as made, not a conflict.
	expect(t, w.replay(w.alice, s, a, "device-a-edit", "edit_expense", map[string]any{"id": id, "description": "late lunch"}, rev(base)), http.StatusOK)
	if revision, _ := w.revisionOf(w.alice, s, a, id); revision != base+2 {
		t.Fatalf("a replayed operation changed the record again: revision %d", revision)
	}
	// The same key for another request is refused, never taken for the first.
	expectCode(t, w.replay(w.alice, s, a, "device-a-edit", "edit_expense", map[string]any{"id": id, "description": "other"}, rev(base)), http.StatusConflict, "app_operation_reused")
}

// A record deleted on the server while a device edited it: the edit is refused as a record the
// app doesn't have, and never brings it back. Restoring is a separate, explicit step that the
// person may take, and the edit is then made on top of the restored revision.
func TestEditOfARecordDeletedMeanwhileDoesNotResurrectIt(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	w.add(w.alice, s, a, "coffee", "5")
	id, base := w.idOf(w.alice, s, a, "lunch")
	expect(t, w.replay(w.owner, s, a, "online-delete", "delete_expense", map[string]any{"id": id}, rev(base)), http.StatusOK)

	rec := w.replay(w.alice, s, a, "offline-edit", "edit_expense", map[string]any{"id": id, "amount": "11"}, rev(base))
	expectCode(t, rec, http.StatusUnprocessableEntity, "app_row_not_found")
	if strings.Contains(rec.Body.String(), "lunch") || strings.Contains(rec.Body.String(), "current") {
		t.Fatalf("the refusal shows a deleted record: %s", rec.Body)
	}
	if rows := w.rowsOf(w.alice, s, a); len(rows) != 1 || rows[0] != "coffee" {
		t.Fatalf("an edit brought a deleted record back: %v", rows)
	}
	// Asking again, with the same key, is refused the same way: nothing is remembered as made.
	expectCode(t, w.replay(w.alice, s, a, "offline-edit", "edit_expense", map[string]any{"id": id, "amount": "11"}, rev(base)), http.StatusUnprocessableEntity, "app_row_not_found")

	// The person chooses to restore: the deletion's revision is in the list of deleted records.
	deleted := decode[Records](t, w.deleted(w.alice, s, a))
	if len(deleted.Records) != 1 || deleted.Records[0].ID != id {
		t.Fatalf("deleted = %+v", deleted)
	}
	restored := decode[Record](t, w.restore(w.alice, s, a, id, rev(deleted.Records[0].Revision)))
	if restored.Revision != deleted.Records[0].Revision+1 {
		t.Fatalf("restored at revision %d, deleted at %d", restored.Revision, deleted.Records[0].Revision)
	}
	// A second restore of the same deletion (the answer was lost) is a conflict that shows the
	// record as it is, and brings nothing back twice.
	rec = w.restore(w.alice, s, a, id, rev(deleted.Records[0].Revision))
	expect(t, rec, http.StatusConflict)
	if decode[conflictReply](t, rec).Current["_revision"] != float64(restored.Revision) {
		t.Fatalf("restore conflict = %s", rec.Body)
	}
	expect(t, w.replay(w.alice, s, a, "offline-edit-after-restore", "edit_expense", map[string]any{"id": id, "amount": "11"}, rev(restored.Revision)), http.StatusOK)
	if revision, amount := w.revisionOf(w.alice, s, a, id); revision != restored.Revision+1 || amount != 11.0 {
		t.Fatalf("after restore and edit: revision %d, amount %v", revision, amount)
	}
}

// A record edited on the server while a device deleted it: the delete is a conflict that shows
// the record as it is, and the record stays. The person may delete it anyway, on top of the
// current revision.
func TestDeleteOfARecordEditedMeanwhileIsAConflict(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	id, base := w.idOf(w.alice, s, a, "lunch")
	expect(t, w.replay(w.owner, s, a, "online-edit", "edit_expense", map[string]any{"id": id, "amount": "20"}, rev(base)), http.StatusOK)

	rec := w.replay(w.alice, s, a, "offline-delete", "delete_expense", map[string]any{"id": id}, rev(base))
	expect(t, rec, http.StatusConflict)
	if reply := decode[conflictReply](t, rec); reply.Code != "app_record_conflict" || reply.Current["amount"] != 20.0 {
		t.Fatalf("conflict = %s", rec.Body)
	}
	if rows := w.rowsOf(w.alice, s, a); len(rows) != 1 {
		t.Fatalf("a conflicting delete removed the record: %v", rows)
	}
	// Explicitly, on top of what the server has: made once, however often it is sent.
	for i := 0; i < 2; i++ {
		expect(t, w.replay(w.alice, s, a, "offline-delete-anyway", "delete_expense", map[string]any{"id": id}, rev(base+1)), http.StatusOK)
	}
	if rows := w.rowsOf(w.alice, s, a); len(rows) != 0 {
		t.Fatalf("rows after the delete = %v", rows)
	}
	// A delete of what is already deleted is not a second deletion: the app doesn't have it.
	expectCode(t, w.replay(w.alice, s, a, "offline-delete-again", "delete_expense", map[string]any{"id": id}, rev(base+1)), http.StatusUnprocessableEntity, "app_row_not_found")
}

// A conflict tells the current record only to someone who may read it, and a record that isn't
// the caller's to change is answered as one the app doesn't have, whatever revision they hold.
func TestOfflineChangesNeverShowWhatTheCallerMayNotRead(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "alice-row", "10")
	id, base := w.idOf(w.alice, s, a, "alice-row")
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppUser), grant(w.bob.id, access.AppReader), grant(w.alice.id, access.AppRoles...))
	for _, u := range []*user{w.owner, w.bob} {
		for _, name := range []string{"edit_expense", "delete_expense"} {
			ctx := map[string]any{"id": id}
			if name == "edit_expense" {
				ctx["amount"] = "1"
			}
			rec := w.replay(u, s, a, fmt.Sprintf("op-%d-%s", u.id, name), name, ctx, rev(base+5))
			if rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusForbidden {
				t.Fatalf("%s by %d = %d: %s", name, u.id, rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "alice-row") || strings.Contains(rec.Body.String(), `"current"`) {
				t.Fatalf("%s by %d shows a record they may not read: %s", name, u.id, rec.Body)
			}
		}
	}
}
