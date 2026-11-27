package apps

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"taskto.ai/taskto/internal/config"
)

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

	w.at(day(89))
	if n := w.purge(); n != 0 {
		t.Fatalf("purged %d records inside the window", n)
	}
	expect(t, w.restore(w.alice, s, a, lunch, nil), http.StatusOK)
	if got := decode[Records](t, w.deleted(w.alice, s, a)); len(got.Records) != 1 || got.Records[0].ID != coffee {
		t.Fatalf("deleted = %+v", got)
	}

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
	expect(t, w.act(w.alice, s, a, "edit_expense", map[string]any{"id": coffee, "amount": "1"}, nil), http.StatusUnprocessableEntity)
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

func TestAnInsertAndDeleteLoopDoesNotGrowWithoutBound(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.e.s.WithDeletedRetention(day(7))
	const days = 40
	base := time.Now()
	for i := 0; i < days; i++ {
		w.e.s.clock = func() time.Time { return base.Add(day(i)) }
		d := fmt.Sprintf("loop-%d", i)
		w.add(w.alice, s, a, d, "1")
		id, _ := w.idOf(w.alice, s, a, d)
		expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": id}, nil), http.StatusOK)
		if _, err := w.e.db.Exec(`UPDATE app_rows SET deleted_at = ? WHERE id = ?`, w.e.s.stamp(w.e.s.clock()), id); err != nil {
			t.Fatal(err)
		}
		w.purge()
		rows := w.count(`SELECT count(*) FROM app_rows WHERE app_id = ?`, a.ID)
		history := w.count(`SELECT count(*) FROM app_row_history WHERE app_id = ?`, a.ID)
		want := min(i+1, 8)
		if rows != want || history != 2*want {
			t.Fatalf("day %d: %d rows and %d history entries are kept, want %d and %d", i, rows, history, want, 2*want)
		}
	}
}

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
	w.add(w.alice, s, a, "newer", "1")

	w.at(day(91))
	if n := w.purge(); n != 1 {
		t.Fatalf("purged %d", n)
	}
	var record *int64
	if err := w.e.db.QueryRow(`SELECT record_id FROM app_operations WHERE operation = 'op-retention-0001'`).Scan(&record); err != nil || record != nil {
		t.Fatalf("the receipt is gone or still names the record: %v, %v", record, err)
	}
	press()
	if n := w.count(`SELECT count(*) FROM app_rows WHERE app_id = ?`, a.ID); n != 1 {
		t.Fatalf("the replay made records: %d rows, want the newer one", n)
	}
}

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
	w.add(w.alice, s, a, "newer", "1")

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
	w.recordWith(w.alice, s, a)

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
	if got := decode[History](t, w.history(w.alice, s, a, id)); len(got.History) == 0 || got.History[0].Revision != int64(newest) {
		t.Fatalf("history = %+v", got.History)
	}
}

func TestAPurgedRecordsIDIsNeverGivenAgain(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.editable(w.alice, s)
	w.add(w.alice, s, a, "first", "1")
	first, _ := w.idOf(w.alice, s, a, "first")
	expect(t, w.act(w.alice, s, a, "delete_expense", map[string]any{"id": first}, nil), http.StatusOK)

	w.at(day(91))
	if n := w.purge(); n != 1 {
		t.Fatalf("purged %d", n)
	}
	w.add(w.alice, s, a, "second", "2")
	second, _ := w.idOf(w.alice, s, a, "second")
	if second <= first {
		t.Fatalf("the new record has id %d, not after %d", second, first)
	}
	if _, err := w.e.db.Exec(`DELETE FROM app_rows`); err != nil {
		t.Fatal(err)
	}
	w.add(w.alice, s, a, "third", "3")
	third, rev := w.idOf(w.alice, s, a, "third")
	if third <= first {
		t.Fatalf("the record after the table emptied has id %d, not after %d", third, first)
	}
	expect(t, w.act(w.alice, s, a, "edit_expense", map[string]any{"id": first, "amount": "9"}, nil), http.StatusUnprocessableEntity)
	if _, now := w.idOf(w.alice, s, a, "third"); now != rev {
		t.Fatalf("the new record is at revision %d, was %d", now, rev)
	}
}

func TestAnInterruptedSweepLeavesNothingDanglingAndTheNextFinishes(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.createPhotos(w.alice, s, "Photos")
	total := retentionBatch + 30
	var last int64
	for i := 0; i < total; i++ {
		res, err := w.e.db.Exec(`INSERT INTO app_rows (app_id, data, created_by, deleted_at) VALUES (?, '{}', ?, ?)`,
			a.ID, w.alice.id, "2020-01-01T00:00:00.000Z")
		if err != nil {
			t.Fatal(err)
		}
		last, _ = res.LastInsertId()
		if _, err := w.e.db.Exec(`INSERT INTO search_records (row_id, text_format) VALUES (?, 1)`, last); err != nil {
			t.Fatal(err)
		}
		if _, err := w.e.db.Exec(`INSERT INTO search_record_fts (rowid, body) VALUES (?, 'words')`, last); err != nil {
			t.Fatal(err)
		}
	}
	id := w.upload(w.alice, s, a, "photos", testPNG(t, 40, 30), 4096)
	row := w.recordWith(w.alice, s, a, id)
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), ClientAction{
		Name: "remove_note", SurfaceID: SurfaceID(a.ID), Context: map[string]any{"id": row}}), http.StatusOK)
	w.recordWith(w.alice, s, a)
	live := w.count(`SELECT count(*) FROM app_rows WHERE app_id = ? AND deleted_at IS NULL`, a.ID)
	w.at(day(91))

	dangling := func() {
		t.Helper()
		if n := w.count(`SELECT count(*) FROM pragma_foreign_key_check`); n != 0 {
			t.Fatalf("%d dangling references", n)
		}
		if n := w.count(`SELECT count(*) FROM search_record_fts WHERE rowid NOT IN (SELECT row_id FROM search_records)`); n != 0 {
			t.Fatalf("%d search entries of removed records", n)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := w.e.s.PurgeDeleted(ctx); err != nil || n != 0 {
		t.Fatalf("a stopped sweep removed %d records: %v", n, err)
	}

	cutoff := w.e.s.stamp(w.e.s.clock().Add(-w.e.s.DeletedRetention()))
	if n, err := w.e.s.purgeBatch(context.Background(), cutoff); err != nil || n != retentionBatch {
		t.Fatalf("a batch removed %d records: %v", n, err)
	}
	dangling()
	if n := w.count(`SELECT count(*) FROM app_rows WHERE deleted_at IS NOT NULL`); n != total+1-retentionBatch {
		t.Fatalf("%d deleted records remain after one batch", n)
	}

	if _, err := w.e.s.PurgeDeleted(context.Background()); err != nil {
		t.Fatal(err)
	}
	dangling()
	if n := w.count(`SELECT count(*) FROM app_rows WHERE deleted_at IS NOT NULL`); n != 0 {
		t.Fatalf("%d deleted records remain", n)
	}
	if n := w.count(`SELECT count(*) FROM app_rows WHERE app_id = ? AND deleted_at IS NULL`, a.ID); n != live {
		t.Fatalf("%d live records remain, want %d", n, live)
	}
	if n := w.count(`SELECT count(*) FROM app_attachments WHERE id = ?`, id); n != 0 {
		t.Fatal("the attachment's row remains")
	}

	if len(w.files(s, a)) == 0 {
		t.Fatal("the files went before the sweep of files")
	}
	for range 2 {
		if err := w.e.s.Sweep(context.Background(), time.Now().Add(AttachmentTTL+time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.files(s, a); len(got) != 0 {
		t.Fatalf("files remain: %v", got)
	}
}
