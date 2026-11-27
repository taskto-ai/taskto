package accountdeletion

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAnOverdueJobDoesNotPurgeARequestWhoseDeadlineIsInTheFuture(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	if err := w.svc.Attach(); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(14 * 24 * time.Hour).UTC().Truncate(time.Millisecond)
	exec(t, w.db, `UPDATE users SET disabled_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, w.leaver)
	exec(t, w.db, `INSERT INTO account_deletions (user_id, requested_at, purge_at) VALUES (?, '2026-01-01T00:00:00.000Z', ?)`,
		w.leaver, future.Format(stampFormat))
	tx, err := w.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.svc.SchedulePurge(ctx, tx, w.leaver, w.ws, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM users WHERE id = ?`, w.leaver); n != 1 {
		t.Fatal("the account was purged before its request's deadline")
	}
	if n := w.count(`SELECT count(*) FROM account_deletions WHERE user_id = ? AND purge_started_at IS NULL`, w.leaver); n != 1 {
		t.Error("the purge began, so the request can no longer be cancelled")
	}
	if len(w.agent.retired) != 0 || len(w.agent.purged) != 0 || len(w.cli.users) != 0 || w.count(`SELECT count(*) FROM purge_cleanups`) != 0 {
		t.Error("something was recorded or removed before the deadline")
	}
	var runAt string
	if err := w.db.QueryRow(`SELECT run_at FROM scheduled_jobs WHERE kind = ? AND user_id = ?`, string(Kind), w.leaver).Scan(&runAt); err != nil {
		t.Fatalf("the job is gone: %v", err)
	}
	if runAt != future.Format(stampFormat) {
		t.Errorf("the job runs at %s, want the request's deadline %s", runAt, future.Format(stampFormat))
	}
}

func TestReconcileUsesTheRequestAsItIsWhenTheJobIsWritten(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	if err := w.svc.Attach(); err != nil {
		t.Fatal(err)
	}
	w.deleteAccount()
	newDeadline := "2027-03-01T00:00:00.000Z"
	w.svc.beforeSchedule = func(user int64) {
		exec(t, w.db, `DELETE FROM account_deletions WHERE user_id = ?`, user)
		exec(t, w.db, `INSERT INTO account_deletions (user_id, requested_at, purge_at) VALUES (?, '2026-11-20T00:00:00.000Z', ?)`, user, newDeadline)
	}
	if err := w.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var runAt string
	if err := w.db.QueryRow(`SELECT run_at FROM scheduled_jobs WHERE kind = ? AND user_id = ?`, string(Kind), w.leaver).Scan(&runAt); err != nil {
		t.Fatalf("no job: %v", err)
	}
	if runAt != newDeadline {
		t.Errorf("the job runs at %s: the old deadline was written over the new request, want %s", runAt, newDeadline)
	}

	w2 := newWorld(t)
	if err := w2.svc.Attach(); err != nil {
		t.Fatal(err)
	}
	w2.deleteAccount()
	w2.svc.beforeSchedule = func(user int64) {
		exec(t, w2.db, `DELETE FROM account_deletions WHERE user_id = ?`, user)
	}
	if err := w2.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if n := w2.count(`SELECT count(*) FROM scheduled_jobs WHERE kind = ?`, string(Kind)); n != 0 {
		t.Errorf("%d jobs for a request that was cancelled", n)
	}

	w3 := newWorld(t)
	if err := w3.svc.Attach(); err != nil {
		t.Fatal(err)
	}
	w3.deleteAccount()
	w3.svc.beforeSchedule = func(user int64) {
		tx, err := w3.db.Begin()
		if err != nil {
			t.Error(err)
			return
		}
		if err := w3.svc.SchedulePurge(ctx, tx, user, w3.ws, time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)); err != nil {
			t.Error(err)
		}
		_ = tx.Commit()
	}
	if err := w3.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var at3 string
	_ = w3.db.QueryRow(`SELECT run_at FROM scheduled_jobs WHERE kind = ? AND user_id = ?`, string(Kind), w3.leaver).Scan(&at3)
	if at3 != "2027-05-01T00:00:00.000Z" {
		t.Errorf("the job of the new request was changed to %s", at3)
	}
}

func TestEachPurgeKeepsItsOwnProvidersStepAgainstAnotherRunsAcknowledgement(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	other := exec(t, w.db, `INSERT INTO users (email, name, password_hash) VALUES ('dave@example.com', 'Dave', 'x')`)
	exec(t, w.db, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, w.ws, other)

	var mu sync.Mutex
	calls := 0
	started, release := make(chan struct{}), make(chan struct{})
	fail := errors.New("the feed can't write")
	w.svc.SetProviders(func(context.Context) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		switch n {
		case 1:
			return fail
		case 2:
			close(started)
			<-release
			return nil
		}
		return fail
	})

	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM purge_cleanups WHERE user_id = ? AND kind = ?`, w.leaver, cleanProviders); n != 1 {
		t.Fatalf("%d provider steps for the first account", n)
	}

	done := make(chan error, 1)
	go func() { done <- w.svc.RunCleanups(ctx) }()
	<-started

	exec(t, w.db, `INSERT INTO account_deletions (user_id, requested_at, purge_at) VALUES (?, '2026-01-01T00:00:00.000Z', '2026-01-15T00:00:00.000Z')`, other)
	if err := w.svc.Purge(ctx, other); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if n := w.count(`SELECT count(*) FROM purge_cleanups WHERE user_id = ? AND kind = ?`, other, cleanProviders); n != 1 {
		t.Fatalf("the second account's providers step was cleared by the first's acknowledgement (%d recorded)", n)
	}
	if n := w.count(`SELECT count(*) FROM purge_cleanups WHERE user_id = ?`, w.leaver); n != 0 {
		t.Errorf("%d steps of the first account remain", n)
	}
	mu.Lock()
	calls = 100
	mu.Unlock()
	w.svc.SetProviders(func(context.Context) error { return nil })
	if err := w.svc.RunCleanups(ctx); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM purge_cleanups`); n != 0 {
		t.Errorf("%d steps remain after the next run", n)
	}
}

func TestAuditCleanupsPendingCountsTheProvidersStep(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	var details string
	if err := w.db.QueryRow(`SELECT details FROM audit_events WHERE action = 'account_purged' LIMIT 1`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(details), &got); err != nil {
		t.Fatal(err)
	}
	if got["cleanups_pending"] != float64(1) {
		t.Errorf("cleanups_pending = %v in %s, want 1", got["cleanups_pending"], details)
	}
}
