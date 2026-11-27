package accountdeletion

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"taskto.ai/taskto/internal/schedule"
)

func TestAPersonAtTheJobBoundCanStillBePurged(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	const theirs schedule.Kind = "test.reminder"
	if err := w.sched.Register(theirs, func(context.Context, schedule.Job) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Attach(); err != nil {
		t.Fatal(err)
	}
	exec(t, w.db, `
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO scheduled_jobs (kind, user_id, workspace_id, dedupe_key, run_at, created_at, updated_at)
		SELECT ?, ?, ?, 'r' || i, '2026-02-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z' FROM n`,
		schedule.MaxPerUser, string(theirs), w.leaver, w.ws)

	err := w.svc.write(ctx, func(tx *sql.Tx) error {
		return w.svc.SchedulePurge(ctx, tx, w.leaver, w.ws, time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))
	})
	if err != nil {
		t.Fatalf("scheduling the purge of a person at the bound: %v", err)
	}

	w.deleteAccount()
	exec(t, w.db, `UPDATE scheduled_jobs SET state = 'failed' WHERE kind = ?`, string(Kind))
	if err := w.svc.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n := w.count(`SELECT count(*) FROM scheduled_jobs WHERE kind = ? AND user_id = ? AND state = 'pending'`, string(Kind), w.leaver); n != 1 {
		t.Fatalf("%d pending purge jobs after reconcile, want 1", n)
	}
}
