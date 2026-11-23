package apps

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
)

func (w *world) download(u *user, s access.Space, a App) *httptest.ResponseRecorder {
	w.e.t.Helper()
	return u.do(http.MethodGet, appPath(s, a.ID, "/offline"), nil)
}

func (w *world) setOffline(admin *user, s access.Space, allowed bool) {
	w.e.t.Helper()
	expect(w.e.t, admin.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/spaces/%d", s.WorkspaceID, s.ID),
		map[string]bool{"offline_downloads": allowed}), http.StatusOK)
}

func TestDownloadHoldsTheLiveReleaseAndWhatTheCallerReads(t *testing.T) {
	w := newWorld(t)
	rec := w.download(w.alice, w.alicePrivate, w.aliceApp)
	expect(t, rec, http.StatusOK)
	got := decode[OfflineApp](t, rec)
	if got.AppID != w.aliceApp.ID || got.Release != w.aliceApp.Live.Number || got.Scope != "all" ||
		got.Records != 1 || got.Total != 1 || got.Truncated || !got.CanInsert || got.CapturedAt == "" {
		t.Fatalf("download = %+v", got)
	}
	if len(got.Messages) != 3 || got.Definition.Actions["add_expense"].Kind != ActionInsert ||
		got.Definition.DefinitionVersion == 0 || got.Definition.Requires == nil {
		t.Fatalf("messages or definition missing: %+v", got)
	}
	// The download is the surface's own messages, so offline draws what online does.
	online := decode[[]Message](t, w.alice.do(http.MethodGet, appPath(w.alicePrivate, w.aliceApp.ID, "/surface"), nil))
	if fmt.Sprint(online) != fmt.Sprint(got.Messages) {
		t.Fatalf("download's messages differ from the surface's:\n%v\n%v", got.Messages, online)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
}

func TestDownloadHoldsOnlyWhatTheCallerMayRead(t *testing.T) {
	w := newWorld(t)
	// A viewer reads the team's records but can't add.
	got := decode[OfflineApp](t, w.download(w.bob, w.shared, w.sharedApp))
	if got.Records != 1 || got.CanInsert {
		t.Fatalf("viewer's download = %+v", got)
	}
	// Someone who reads only their own records is sent only those.
	w.mustSetRoles(w.alice, w.shared, w.sharedApp, grant(w.owner.id, access.AppUser), grant(w.alice.id, access.AppRoles...))
	expect(t, w.add2(w.owner, w.shared, w.sharedApp, "owners-own"), http.StatusOK)
	own := w.download(w.owner, w.shared, w.sharedApp)
	expect(t, own, http.StatusOK)
	d := decode[OfflineApp](t, own)
	if d.Scope != "own" || d.Records != 1 || d.Total != 1 {
		t.Fatalf("own-scope download = %+v", d)
	}
	if strings.Contains(own.Body.String(), "team-lunch") {
		t.Fatalf("the download holds a record the caller doesn't read: %s", own.Body)
	}
}

func TestDownloadLeavesOutDeletedRecordsAndUndeclaredFields(t *testing.T) {
	w := newWorld(t)
	w.add(w.alice, w.alicePrivate, w.aliceApp, "to-delete", "5")
	var id int64
	if err := w.e.db.QueryRow(`SELECT max(id) FROM app_rows WHERE app_id = ?`, w.aliceApp.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	expect(t, w.alice.do(http.MethodPost, appPath(w.alicePrivate, w.aliceApp.ID, "/actions"),
		action(w.aliceApp, "delete_expense", map[string]any{"id": id})), http.StatusOK)
	// A value the collection doesn't declare stays stored, and is never sent.
	if _, err := w.e.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.hidden', 'never-sent') WHERE app_id = ?`, w.aliceApp.ID); err != nil {
		t.Fatal(err)
	}
	rec := w.download(w.alice, w.alicePrivate, w.aliceApp)
	expect(t, rec, http.StatusOK)
	if body := rec.Body.String(); strings.Contains(body, "to-delete") || strings.Contains(body, "never-sent") {
		t.Fatalf("the download holds a deleted record or an undeclared field: %s", body)
	}
	if got := decode[OfflineApp](t, rec); got.Records != 1 {
		t.Fatalf("records = %d", got.Records)
	}
}

func TestDownloadIsBounded(t *testing.T) {
	w := newWorld(t)
	for i := 0; i < MaxOfflineRows+3; i++ {
		if _, err := w.e.db.Exec(`INSERT INTO app_rows (app_id, data, created_by, updated_by) VALUES (?, ?, ?, ?)`,
			w.aliceApp.ID, fmt.Sprintf(`{"description":"row-%d","amount":1}`, i), w.alice.id, w.alice.id); err != nil {
			t.Fatal(err)
		}
	}
	rec := w.download(w.alice, w.alicePrivate, w.aliceApp)
	expect(t, rec, http.StatusOK)
	got := decode[OfflineApp](t, rec)
	if got.Records != MaxOfflineRows || got.Total != MaxOfflineRows+4 || !got.Truncated {
		t.Fatalf("download = records %d, total %d, truncated %v", got.Records, got.Total, got.Truncated)
	}
	// The latest are kept.
	if !strings.Contains(rec.Body.String(), fmt.Sprintf("row-%d", MaxOfflineRows+2)) || strings.Contains(rec.Body.String(), `"row-0"`) {
		t.Fatal("the download isn't the latest records")
	}
}

func TestAnAdminCanRefuseOfflineDownloadsForASpace(t *testing.T) {
	w := newWorld(t)
	expect(t, w.download(w.alice, w.shared, w.sharedApp), http.StatusOK)
	w.setOffline(w.owner, w.shared, false)
	expectCode(t, w.download(w.alice, w.shared, w.sharedApp), http.StatusForbidden, "offline_downloads_disabled")
	expectCode(t, w.download(w.bob, w.shared, w.sharedApp), http.StatusForbidden, "offline_downloads_disabled")
	// Everything else about the app is as it was: online use is not offline use.
	expect(t, w.alice.do(http.MethodGet, appPath(w.shared, w.sharedApp.ID, "/surface"), nil), http.StatusOK)
	w.add(w.alice, w.shared, w.sharedApp, "online", "1")
	// The setting is the space's, and every reader of the space sees it.
	got := decode[access.Space](t, w.bob.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces/%d", w.ws, w.shared.ID), nil))
	if got.OfflineDownloads {
		t.Fatalf("space = %+v", got)
	}
	// Only an admin changes it.
	expect(t, w.alice.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/spaces/%d", w.ws, w.shared.ID),
		map[string]bool{"offline_downloads": true}), http.StatusForbidden)
	// And it can be allowed again.
	w.setOffline(w.owner, w.shared, true)
	expect(t, w.download(w.alice, w.shared, w.sharedApp), http.StatusOK)
	// A private space always allows it, and has no setting.
	expect(t, w.alice.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/spaces/%d", w.ws, w.alicePrivate.ID),
		map[string]bool{"offline_downloads": false}), http.StatusForbidden)
	if !w.e.private(w.alice, w.ws).OfflineDownloads {
		t.Fatal("a private space refused downloads")
	}
}

func TestDownloadChecksAccessAndState(t *testing.T) {
	w := newWorld(t)
	// Outsiders: another's private space, a shared space they aren't in, another workspace.
	expect(t, w.download(w.bob, w.alicePrivate, w.aliceApp), http.StatusNotFound)
	expect(t, w.download(w.owner, w.alicePrivate, w.aliceApp), http.StatusNotFound)
	expect(t, w.download(w.mallory, w.shared, w.sharedApp), http.StatusNotFound)
	expect(t, w.download(w.stranger, w.shared, w.sharedApp), http.StatusNotFound)
	// An app that was never published has nothing to download; one taken offline can't be.
	draft := w.draft(w.alice, w.alicePrivate, "Draft only")
	expectCode(t, w.download(w.alice, w.alicePrivate, draft), http.StatusConflict, "app_not_published")
	expect(t, w.alice.do(http.MethodPatch, appPath(w.alicePrivate, w.aliceApp.ID, ""),
		map[string]any{"version": w.aliceApp.Version, "archived": true}), http.StatusOK)
	expectCode(t, w.download(w.alice, w.alicePrivate, w.aliceApp), http.StatusConflict, "app_offline")
	// No session: refused, and a download is only ever a GET.
	anonymous := &user{e: w.e}
	expect(t, anonymous.do(http.MethodGet, appPath(w.alicePrivate, w.aliceApp.ID, "/offline"), nil), http.StatusUnauthorized)
	expect(t, w.alice.do(http.MethodPost, appPath(w.alicePrivate, w.aliceApp.ID, "/offline"), map[string]any{}), http.StatusMethodNotAllowed)
}

// An insert made offline is sent again until the server answers: the operation key makes the
// repeats apply once, and its receipt is committed with the record.
func TestAReplayedInsertMakesOneRecord(t *testing.T) {
	w := newWorld(t)
	a := w.aliceApp
	send := func(op string, amount string) *httptest.ResponseRecorder {
		act := action(a, "add_expense", map[string]any{"description": "from the train", "amount": amount})
		act.Operation = op
		rel := a.Live.Number
		act.Release = &rel
		return w.alice.do(http.MethodPost, appPath(w.alicePrivate, a.ID, "/actions"), act)
	}
	before := w.rowCount(a)
	first := send("offline-op-0001", "12")
	expect(t, first, http.StatusOK)
	for i := 0; i < 3; i++ {
		again := send("offline-op-0001", "12")
		expect(t, again, http.StatusOK)
		if again.Body.String() != first.Body.String() {
			t.Fatalf("a replay answered differently:\n%s\n%s", again.Body, first.Body)
		}
	}
	if got := w.rowCount(a); got != before+1 {
		t.Fatalf("rows = %d, want %d: a replay made another record", got, before+1)
	}
	// The same key for another request is refused and changes nothing.
	expectCode(t, send("offline-op-0001", "13"), http.StatusConflict, "app_operation_reused")
	if got := w.rowCount(a); got != before+1 {
		t.Fatalf("rows = %d after a reused key", got)
	}
	// The key belongs to its user and app: another user's same key is their own request.
	var receipts int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM app_operations WHERE app_id = ? AND operation = ?`, a.ID, "offline-op-0001").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 {
		t.Fatalf("receipts = %d", receipts)
	}
}

// The receipt and the record are one transaction: an insert that is refused leaves no receipt, so
// a corrected request under the same key is made, and a record that was made always has its receipt.
func TestTheReceiptIsCommittedWithTheRecordOrNotAtAll(t *testing.T) {
	w := newWorld(t)
	a := w.aliceApp
	rows, receipts := w.rowCount(a), func() int {
		var n int
		if err := w.e.db.QueryRow(`SELECT count(*) FROM app_operations WHERE app_id = ?`, a.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	was := receipts()
	bad := action(a, "add_expense", map[string]any{"description": "no amount", "amount": "not a number"})
	bad.Operation = "offline-op-0002"
	expectCode(t, w.alice.do(http.MethodPost, appPath(w.alicePrivate, a.ID, "/actions"), bad), http.StatusUnprocessableEntity, "app_field_not_number")
	if w.rowCount(a) != rows || receipts() != was {
		t.Fatalf("a refused insert left rows %d→%d, receipts %d→%d", rows, w.rowCount(a), was, receipts())
	}
	good := action(a, "add_expense", map[string]any{"description": "fixed", "amount": "3"})
	good.Operation = "offline-op-0002"
	expect(t, w.alice.do(http.MethodPost, appPath(w.alicePrivate, a.ID, "/actions"), good), http.StatusOK)
	if w.rowCount(a) != rows+1 || receipts() != was+1 {
		t.Fatalf("rows %d→%d, receipts %d→%d", rows, w.rowCount(a), was, receipts())
	}
	// Both are in the same commit: a record without its receipt can't be told from a new request.
	var orphans int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM app_rows r WHERE r.app_id = ? AND r.id > 0 AND json_extract(r.data, '$.description') = 'fixed'
		AND NOT EXISTS (SELECT 1 FROM app_operations o WHERE o.app_id = r.app_id AND o.operation = 'offline-op-0002')`, a.ID).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("orphans = %d, err %v", orphans, err)
	}
}

// A replay of an insert that names the release its screen was drawn from: the live release takes
// it if it can, and says the release is outdated if it can't, creating nothing.
func TestAReplayNamesItsReleaseAndAnOutdatedOneIsRefused(t *testing.T) {
	w := newWorld(t)
	a := w.aliceApp
	old := a.Live.Number
	// A newer release that requires a field the old screen doesn't send.
	def := example(t)
	def.Collection.Fields = append(def.Collection.Fields, Field{Name: "category", Type: TypeString, Required: true})
	rec := w.alice.do(http.MethodPatch, appPath(w.alicePrivate, a.ID, ""), map[string]any{"version": a.Version, "definition": def})
	expect(t, rec, http.StatusOK)
	updated := decode[App](t, rec)
	rec = w.alice.do(http.MethodPost, appPath(w.alicePrivate, a.ID, "/publish"), map[string]any{"version": updated.Version, "acknowledge": []any{map[string]string{"change": ChangeFieldRequired, "field": "category"}}})
	expect(t, rec, http.StatusOK)
	before := w.rowCount(a)
	act := action(a, "add_expense", map[string]any{"description": "captured offline", "amount": "9"})
	act.Operation = "offline-op-0003"
	act.Release = &old
	rec = w.alice.do(http.MethodPost, appPath(w.alicePrivate, a.ID, "/actions"), act)
	if rec.Code == http.StatusOK {
		t.Fatalf("an insert missing the new required field was accepted: %s", rec.Body)
	}
	if w.rowCount(a) != before {
		t.Fatal("a refused replay made a record")
	}
}

// A queued insert is sent only once its photos are finalized, and the server refuses one that
// names a photo that isn't: no record ever references media that isn't there.
func TestAnInsertNeverReferencesAPhotoThatIsNotFinalized(t *testing.T) {
	w := newWorld(t)
	a := w.createPhotos(w.alice, w.alicePrivate, "Notes")
	png := testPNG(t, 16, 16)
	id := w.start(w.alice, w.alicePrivate, a, "photos", len(png))
	w.send(w.alice, w.alicePrivate, a, id, png[:len(png)/2], 64)
	insert := func(op string) *httptest.ResponseRecorder {
		act := action(a, "add_note", map[string]any{"title": "with photo", "photos": []any{id}})
		act.Operation = op
		return w.alice.do(http.MethodPost, appPath(w.alicePrivate, a.ID, "/actions"), act)
	}
	expectCode(t, insert("offline-op-0004"), http.StatusUnprocessableEntity, "app_attachment_bad")
	if w.rowCount(a) != 0 {
		t.Fatal("a record references an unfinished upload")
	}
	// The upload resumes where the server is, finalizes, and then the same operation is made once.
	w.send2(w.alice, w.alicePrivate, a, id, png, len(png)/2, 64)
	expect(t, w.alice.do(http.MethodPost, appPath(w.alicePrivate, a.ID, "/uploads/"+id+"/finalize"), map[string]any{}), http.StatusOK)
	expect(t, insert("offline-op-0004"), http.StatusOK)
	expect(t, insert("offline-op-0004"), http.StatusOK)
	if w.rowCount(a) != 1 {
		t.Fatalf("rows = %d", w.rowCount(a))
	}
}

// send2 sends data from offset on.
func (w *world) send2(u *user, s access.Space, a App, id string, data []byte, offset, chunk int) {
	w.e.t.Helper()
	for off := offset; off < len(data); off += chunk {
		expect(w.e.t, w.chunk(u, s, a, id, off, data[off:min(off+chunk, len(data))]), http.StatusOK)
	}
}

// A queued record is sent later, when the browser's session may be another account's: a request
// that names the account it is for is refused for any other, before anything is read or written
// as them, and before the space is looked at (a 404 would say what they may see).
func TestARequestForAnotherAccountIsRefusedBeforeItDoesAnything(t *testing.T) {
	w := newWorld(t)
	w.mustSetRoles(w.alice, w.shared, w.sharedApp, grant(w.alice.id, access.AppRoles...))
	as := func(id int64) map[string]string {
		return map[string]string{access.ExpectedUserHeader: fmt.Sprint(id)}
	}
	rows := w.rowCount(w.sharedApp)
	act := action(w.sharedApp, "add_expense", map[string]any{"description": "not mine", "amount": "1"})
	act.Operation = "offline-op-9001"

	// The session is alice's; the queue was the owner's.
	rec := w.alice.doWith(as(w.owner.id), http.MethodPost, appPath(w.shared, w.sharedApp.ID, "/actions"), act)
	expectCode(t, rec, http.StatusConflict, "session_account_changed")
	if w.rowCount(w.sharedApp) != rows {
		t.Fatal("a record was made as the wrong account")
	}
	// Photo uploads are the same.
	expectCode(t, w.alice.doWith(as(w.owner.id), http.MethodPost, appPath(w.shared, w.sharedApp.ID, "/uploads"),
		map[string]any{"field": "photos", "size": 10}), http.StatusConflict, "session_account_changed")
	// Reads too, and a space the session can't open is no different: not a 404.
	expectCode(t, w.bob.doWith(as(w.alice.id), http.MethodGet, appPath(w.alicePrivate, w.aliceApp.ID, ""), nil),
		http.StatusConflict, "session_account_changed")
	// Without a session it is the usual 401, and a value that is no account's is a 400.
	anon := &user{e: w.e}
	expect(t, anon.doWith(as(w.alice.id), http.MethodGet, appPath(w.shared, w.sharedApp.ID, ""), nil), http.StatusUnauthorized)
	expectCode(t, w.alice.doWith(map[string]string{access.ExpectedUserHeader: "someone"}, http.MethodGet,
		appPath(w.shared, w.sharedApp.ID, ""), nil), http.StatusBadRequest, "bad_request")

	// Named for the session's own account, or not named, the request is as it always was.
	expect(t, w.alice.doWith(as(w.alice.id), http.MethodPost, appPath(w.shared, w.sharedApp.ID, "/actions"), act), http.StatusOK)
	if w.rowCount(w.sharedApp) != rows+1 {
		t.Fatal("the request for the signed-in account made no record")
	}
}

// A download says which of its records the caller's own operations made, so a device that sent a
// record and then downloads again doesn't count it twice (once from its outbox, once as the
// server's). Only the caller's, and only records the download holds.
func TestADownloadSaysWhichRecordsTheCallersOperationsMade(t *testing.T) {
	w := newWorld(t)
	a, s := w.aliceApp, w.alicePrivate
	send := func(op, description string) {
		act := action(a, "add_expense", map[string]any{"description": description, "amount": "4"})
		act.Operation = op
		expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), act), http.StatusOK)
	}
	send("offline-op-0101", "kept")
	send("offline-op-0102", "later deleted")
	var gone int64
	if err := w.e.db.QueryRow(`SELECT record_id FROM app_operations WHERE operation = 'offline-op-0102'`).Scan(&gone); err != nil || gone == 0 {
		t.Fatalf("the operation doesn't remember its record: %d, %v", gone, err)
	}
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"),
		action(a, "delete_expense", map[string]any{"id": gone})), http.StatusOK)

	got := decode[OfflineApp](t, w.download(w.alice, s, a))
	var kept int64
	if err := w.e.db.QueryRow(`SELECT record_id FROM app_operations WHERE operation = 'offline-op-0101'`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if len(got.Operations) != 1 || got.Operations["offline-op-0101"] != kept {
		t.Fatalf("operations = %v, want only offline-op-0101 → %d", got.Operations, kept)
	}
	// Someone else's download never names it.
	w.mustSetRoles(w.alice, w.shared, w.sharedApp, grant(w.alice.id, access.AppRoles...))
	act := action(w.sharedApp, "add_expense", map[string]any{"description": "alice's", "amount": "1"})
	act.Operation = "offline-op-0103"
	expect(t, w.alice.do(http.MethodPost, appPath(w.shared, w.sharedApp.ID, "/actions"), act), http.StatusOK)
	if ops := decode[OfflineApp](t, w.download(w.bob, w.shared, w.sharedApp)).Operations; len(ops) != 0 {
		t.Fatalf("bob's download names alice's operations: %v", ops)
	}
	if ops := decode[OfflineApp](t, w.download(w.alice, w.shared, w.sharedApp)).Operations; len(ops) != 1 {
		t.Fatalf("alice's download = %v", ops)
	}
}
