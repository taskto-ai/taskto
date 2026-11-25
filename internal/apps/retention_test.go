package apps

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"taskto.ai/taskto/internal/config"
)

// at sets the service's clock to d after the real time now.
func (w *world) at(d time.Duration) {
	w.e.t.Helper()
	base := time.Now()
	w.e.s.clock = func() time.Time { return base.Add(d) }
}

func (w *world) count(query string, args ...any) int {
	w.e.t.Helper()
	var n int
	if err := w.e.db.QueryRow(query, args...).Scan(&n); err != nil {
		w.e.t.Fatal(err)
	}
	return n
}

func (w *world) purge() int {
	w.e.t.Helper()
	n, err := w.e.s.PurgeDeleted(context.Background())
	if err != nil {
		w.e.t.Fatal(err)
	}
	return n
}

func day(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

func TestTheDefaultRetentionIsTheConfigsDefault(t *testing.T) {
	if got := (config.Config{}).AppDeletedRetention(); got != DefaultDeletedRetention {
		t.Fatalf("config says %v, apps %v", got, DefaultDeletedRetention)
	}
	if got := newEnv(t).s.DeletedRetention(); got != DefaultDeletedRetention {
		t.Fatalf("a new service keeps deleted records for %v", got)
	}
}

// A deleted record is restorable inside the window; after it the record and its history are gone,
// and a live record, with its history, is never touched.
func TestDeletedRecordsAreRemovedAfterTheirRetention(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	w.add(w.alice, s, a, "coffee", "5")
	w.add(w.alice, s, a, "tea", "2")
	lunch, _ := w.idOf(w.alice, s, a, "lunch")
	coffee, _ := w.idOf(w.alice, s, a, "coffee")
	tea, _ := w.idOf(w.alice, s, a, "tea")
	expect(t, w.act(w.owner, s, a, "edit_expense", map[string]any{"id": tea, "amount": "3"}, nil), http.StatusOK)
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": lunch}, nil), http.StatusOK)
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": coffee}, nil), http.StatusOK)

	// Inside the window nothing goes, and a record is restored as ever.
	w.at(day(89))
	if n := w.purge(); n != 0 {
		t.Fatalf("purged %d records inside the window", n)
	}
	expect(t, w.restore(w.alice, s, a, lunch, nil), http.StatusOK)
	if got := decode[Records](t, w.deleted(w.alice, s, a)); len(got.Records) != 1 || got.Records[0].ID != coffee {
		t.Fatalf("deleted = %+v", got)
	}

	// After it, the deleted record and its history are gone; the restored one and the live one
	// are not, whatever their age.
	w.at(day(91))
	if n := w.purge(); n != 1 {
		t.Fatalf("purged %d records, want the one still deleted", n)
	}
	if got := decode[Records](t, w.deleted(w.alice, s, a)); len(got.Records) != 0 {
		t.Fatalf("deleted = %+v", got)
	}
	expect(t, w.restore(w.alice, s, a, coffee, nil), http.StatusNotFound)
	expect(t, w.history(w.alice, s, a, coffee), http.StatusNotFound)
	if n := w.count(`SELECT count(*) FROM app_row_history WHERE row_id = ?`, coffee); n != 0 {
		t.Fatalf("%d history entries of a purged record remain", n)
	}
	if got := operations(decode[History](t, w.history(w.alice, s, a, tea))); got != "2:update 1:insert" {
		t.Fatalf("the live record's history = %q", got)
	}
	if got := operations(decode[History](t, w.history(w.alice, s, a, lunch))); got != "3:restore 2:delete 1:insert" {
		t.Fatalf("the restored record's history = %q", got)
	}
	// A purged record is as one that never was, for a change too.
	expect(t, w.act(w.alice, s, a, "edit_expense", map[string]any{"id": coffee, "amount": "1"}, nil), http.StatusUnprocessableEntity)
	// And nothing is left to purge.
	if n := w.purge(); n != 0 {
		t.Fatalf("purged %d more", n)
	}
}

func TestRetentionOffKeepsDeletedRecordsForEver(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.e.s.WithDeletedRetention(0)
	w.add(w.alice, s, a, "lunch", "10")
	id, _ := w.idOf(w.alice, s, a, "lunch")
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": id}, nil), http.StatusOK)
	w.at(day(36500))
	if n := w.purge(); n != 0 {
		t.Fatalf("purged %d records with retention off", n)
	}
	expect(t, w.restore(w.alice, s, a, id, nil), http.StatusOK)
}

// A loop of inserts and deletes, however long it runs, leaves the database a window's worth of
// records, not all of them.
func TestAnInsertAndDeleteLoopDoesNotGrowWithoutBound(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.e.s.WithDeletedRetention(day(7))
	const days = 40
	for i := 0; i < days; i++ {
		w.at(day(i))
		d := fmt.Sprintf("loop-%d", i)
		w.add(w.alice, s, a, d, "1")
		id, _ := w.idOf(w.alice, s, a, d)
		expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": id}, nil), http.StatusOK)
		w.purge()
		rows := w.count(`SELECT count(*) FROM app_rows WHERE app_id = ?`, a.ID)
		history := w.count(`SELECT count(*) FROM app_row_history WHERE app_id = ?`, a.ID)
		// The window holds a week of days, and each record has two history entries.
		if rows > 8 || history > 16 {
			t.Fatalf("day %d: %d rows and %d history entries are kept", i, rows, history)
		}
	}
}

// More than a batch is removed, in as many as it takes.
func TestPurgeRemovesMoreThanOneBatch(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	total := 2*retentionBatch + 50
	for i := 0; i < total; i++ {
		if _, err := w.e.db.Exec(`INSERT INTO app_rows (app_id, data, created_by, deleted_at) VALUES (?, '{}', ?, ?)`,
			a.ID, w.alice.id, "2020-01-01T00:00:00.000Z"); err != nil {
			t.Fatal(err)
		}
	}
	w.add(w.alice, s, a, "live", "1")
	if n := w.purge(); n != total {
		t.Fatalf("purged %d, want %d", n, total)
	}
	if n := w.count(`SELECT count(*) FROM app_rows WHERE app_id = ?`, a.ID); n != 1 {
		t.Fatalf("%d rows remain, want the live one", n)
	}
}

// The receipt of an operation outlives the record it made, so a device that replays an old
// request is answered as it was while the receipt is kept, and not made a second record.
func TestAReceiptOutlivesItsPurgedRecord(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	live := w.view(w.alice, s, a).Live.Number
	press := func() {
		act := action(a, "add_expense", map[string]any{"description": "Lunch", "amount": "3"})
		act.Operation = "op-retention-0001"
		act.Release = &live
		expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), act), http.StatusOK)
	}
	press()
	id, _ := w.idOf(w.alice, s, a, "Lunch")
	if _, err := w.e.db.Exec(`UPDATE app_operations SET record_id = ? WHERE operation = 'op-retention-0001'`, id); err != nil {
		t.Fatal(err)
	}
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": id}, nil), http.StatusOK)

	w.at(day(91))
	if n := w.purge(); n != 1 {
		t.Fatalf("purged %d", n)
	}
	var record *int64
	if err := w.e.db.QueryRow(`SELECT record_id FROM app_operations WHERE operation = 'op-retention-0001'`).Scan(&record); err != nil || record != nil {
		t.Fatalf("the receipt is gone or still names the record: %v, %v", record, err)
	}
	press() // answered from the receipt: no second record
	if n := w.count(`SELECT count(*) FROM app_rows WHERE app_id = ?`, a.ID); n != 0 {
		t.Fatalf("the replay made %d records", n)
	}
}

// A record with an effect that waits is kept until it has run; its finished effects go with it.
func TestARecordWithAnEffectThatWaitsIsKept(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "10")
	id, _ := w.idOf(w.alice, s, a, "lunch")
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": id}, nil), http.StatusOK)
	res, err := w.e.db.Exec(`INSERT INTO app_operations (app_id, user_id, operation, action, request_hash, record_id) VALUES (?, ?, 'op-retention-0002', 'add_expense', 'x', ?)`,
		a.ID, w.alice.id, id)
	if err != nil {
		t.Fatal(err)
	}
	op, _ := res.LastInsertId()
	effect, err := w.e.db.Exec(`INSERT INTO app_effects (app_id, operation_id, position, kind, record_id, record_revision, user_id, action_kind, payload, state, next_attempt_at)
		VALUES (?, ?, 0, 'create_card', ?, 1, ?, 'insert', '{}', 'pending', '2999-01-01T00:00:00.000Z')`, a.ID, op, id, w.alice.id)
	if err != nil {
		t.Fatal(err)
	}
	effectID, _ := effect.LastInsertId()

	w.at(day(91))
	if n := w.purge(); n != 0 {
		t.Fatalf("purged a record whose effect waits")
	}
	if _, err := w.e.db.Exec(`UPDATE app_effects SET state = 'done' WHERE id = ?`, effectID); err != nil {
		t.Fatal(err)
	}
	if n := w.purge(); n != 1 {
		t.Fatalf("purged %d once the effect was done", n)
	}
	if n := w.count(`SELECT count(*) FROM app_effects WHERE id = ?`, effectID); n != 0 {
		t.Fatalf("the finished effect of a purged record remains")
	}
	if n := w.count(`SELECT count(*) FROM app_operations WHERE id = ?`, op); n != 1 {
		t.Fatalf("the operation's receipt went with the record")
	}
}

// The photos of a purged record go with it: its rows at once, its files at the next sweep of
// files no row knows.
func TestAPurgedRecordsPhotosAreSweptAway(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.createPhotos(w.alice, s, "Photos")
	id := w.upload(w.alice, s, a, "photos", testPNG(t, 40, 30), 4096)
	row := w.recordWith(w.alice, s, a, id)
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), ClientAction{
		Name: "remove_note", SurfaceID: SurfaceID(a.ID), Context: map[string]any{"id": row}}), http.StatusOK)
	if len(w.files(s, a)) == 0 {
		t.Fatal("no files were stored")
	}

	w.at(day(91))
	if n := w.purge(); n != 1 {
		t.Fatalf("purged %d", n)
	}
	if n := w.count(`SELECT count(*) FROM app_attachments WHERE id = ?`, id); n != 0 {
		t.Fatal("the attachment's row remains")
	}
	if err := w.e.s.Sweep(context.Background(), time.Now().Add(AttachmentTTL+time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := w.files(s, a); len(got) != 0 {
		t.Fatalf("files remain: %v", got)
	}
}

// A record edited in a loop keeps its newest versions only.
func TestAHistoryKeepsItsNewestRevisions(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "lunch", "1")
	id, _ := w.idOf(w.alice, s, a, "lunch")
	for i := 0; i < MaxRevisions+20; i++ {
		expect(t, w.act(w.alice, s, a, "edit_expense", map[string]any{"id": id, "amount": fmt.Sprint(i + 2)}, nil), http.StatusOK)
	}
	var oldest, newest, n int
	if err := w.e.db.QueryRow(`SELECT min(revision), max(revision), count(*) FROM app_row_history WHERE row_id = ?`, id).Scan(&oldest, &newest, &n); err != nil {
		t.Fatal(err)
	}
	if n != MaxRevisions || newest != MaxRevisions+21 || oldest != newest-MaxRevisions+1 {
		t.Fatalf("history holds %d versions, %d to %d", n, oldest, newest)
	}
	// The record itself and its history's route are as ever.
	if got := decode[History](t, w.history(w.alice, s, a, id)); len(got.History) == 0 || got.History[0].Revision != int64(newest) {
		t.Fatalf("history = %+v", got.History)
	}
}
