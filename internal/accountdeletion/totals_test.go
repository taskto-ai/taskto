package accountdeletion

import (
	"context"
	"encoding/json"
	"testing"
)

// purgedDetails is the account_purged event's details.
func (w *world) purgedDetails() map[string]any {
	w.t.Helper()
	var details string
	if err := w.db.QueryRow(`SELECT details FROM audit_events WHERE action = 'account_purged' LIMIT 1`).Scan(&details); err != nil {
		w.t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(details), &got); err != nil {
		w.t.Fatal(err)
	}
	return got
}

// A purge that took two tries reports what both did: the first removed the unfinished upload and
// rewrote the team space's history, then failed on another shared space; the second finds both done
// and counts nothing for them.
func TestPurgeTotalsAreKeptAcrossItsTries(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	app, _ := w.newApp(w.team.ID, "Site visits")
	w.attachment(photoA, app, w.team.ID, w.leaver, 0)
	w.putFile(w.team, app, photoA+".part", "half a picture")
	other := exec(t, w.db, `INSERT INTO spaces (workspace_id, kind, name) VALUES (?, 'shared', 'Other')`, w.ws)
	kb := &failingKB{Knowledge: w.kb, failIDs: map[int64]bool{other: true}}
	w.svc.o.Knowledge = kb
	w.deleteAccount()

	if err := w.svc.Purge(ctx, w.leaver); err == nil {
		t.Fatal("the first try went through a failed rewrite")
	}
	if n := w.count(`SELECT count(*) FROM app_attachments WHERE id = ?`, photoA); n != 0 {
		t.Fatal("fixture: the first try didn't remove the upload")
	}
	kb.failIDs = nil
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}

	got := w.purgedDetails()
	if got["uploads_removed"] != float64(1) {
		t.Errorf("uploads_removed = %v, want 1 (removed by the first try)", got["uploads_removed"])
	}
	// The team space has two commits of theirs (plan.md's first version and minutes.md).
	if got["documents_anonymized"] != float64(2) {
		t.Errorf("documents_anonymized = %v, want 2 (rewritten by the first try)", got["documents_anonymized"])
	}
}

// A purge in one try reports the same totals.
func TestPurgeTotalsInOneTry(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	app, _ := w.newApp(w.team.ID, "Site visits")
	w.attachment(photoA, app, w.team.ID, w.leaver, 0)
	w.putFile(w.team, app, photoA+".part", "half a picture")
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	got := w.purgedDetails()
	if got["uploads_removed"] != float64(1) || got["documents_anonymized"] != float64(2) {
		t.Errorf("uploads_removed = %v, documents_anonymized = %v, want 1 and 2", got["uploads_removed"], got["documents_anonymized"])
	}
}
