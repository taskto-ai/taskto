package accountdeletion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"taskto.ai/taskto/internal/access"
)

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
	if got["documents_anonymized"] != float64(2) {
		t.Errorf("documents_anonymized = %v, want 2 (rewritten by the first try)", got["documents_anonymized"])
	}
}

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

type stopsAfterKB struct {
	Knowledge
	stop bool
}

func (k *stopsAfterKB) AnonymizeAuthorsNoting(ctx context.Context, sp access.Space, emails []string, planned func(int) error) (int, error) {
	n, err := k.Knowledge.AnonymizeAuthorsNoting(ctx, sp, emails, planned)
	if err == nil && k.stop {
		return n, errors.New("stopped after the new history was published")
	}
	return n, err
}

type stopsAfterApps struct {
	Apps
	stop bool
}

func (a *stopsAfterApps) PurgeUploadsNoting(ctx context.Context, owner int64, removed func(context.Context, *sql.Tx) error) (int, error) {
	n, err := a.Apps.PurgeUploadsNoting(ctx, owner, removed)
	if err == nil && a.stop {
		return n, errors.New("stopped after removing some")
	}
	return n, err
}

func TestPurgeTotalsSurviveAStepThatFailsAfterItsWork(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	app, _ := w.newApp(w.team.ID, "Site visits")
	w.attachment(photoA, app, w.team.ID, w.leaver, 0)
	w.putFile(w.team, app, photoA+".part", "half a picture")
	apps := &stopsAfterApps{Apps: w.apps, stop: true}
	kb := &stopsAfterKB{Knowledge: w.kb, stop: true}
	w.svc.SetApps(apps)
	w.svc.o.Knowledge = kb
	w.deleteAccount()

	if err := w.svc.Purge(ctx, w.leaver); err == nil {
		t.Fatal("the uploads' failure was passed over")
	}
	apps.stop = false
	if err := w.svc.Purge(ctx, w.leaver); err == nil {
		t.Fatal("the rewrite's failure was passed over")
	}
	kb.stop = false
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	got := w.purgedDetails()
	if got["uploads_removed"] != float64(1) || got["documents_anonymized"] != float64(2) {
		t.Errorf("uploads_removed = %v, documents_anonymized = %v, want 1 and 2", got["uploads_removed"], got["documents_anonymized"])
	}
}

type stopsBeforeJournalKB struct {
	Knowledge
	stop bool
}

func (k *stopsBeforeJournalKB) AnonymizeAuthorsNoting(ctx context.Context, sp access.Space, emails []string, planned func(int) error) (int, error) {
	return k.Knowledge.AnonymizeAuthorsNoting(ctx, sp, emails, func(n int) error {
		if err := planned(n); err != nil {
			return err
		}
		if k.stop {
			return errors.New("stopped before the rewrite was journaled")
		}
		return nil
	})
}

func TestARewriteCountedTwiceBeforeItsJournalIsCountedOnce(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	kb := &stopsBeforeJournalKB{Knowledge: w.kb, stop: true}
	w.svc.o.Knowledge = kb
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err == nil {
		t.Fatal("the stopped rewrite was passed over")
	}
	if n := w.count(`SELECT commits FROM purge_anonymized WHERE user_id = ? AND space_id = ?`, w.leaver, w.team.ID); n != 2 {
		t.Fatalf("fixture: the stopped try kept %d, want 2", n)
	}
	kb.stop = false
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if got := w.purgedDetails()["documents_anonymized"]; got != float64(2) {
		t.Errorf("documents_anonymized = %v, want 2", got)
	}
}
