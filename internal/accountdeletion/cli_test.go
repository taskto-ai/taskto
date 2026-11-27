package accountdeletion

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func (w *world) cliSteps() []string {
	w.t.Helper()
	rows, err := w.db.Query(`SELECT target FROM purge_cleanups WHERE kind = ? AND user_id = ? ORDER BY target`, cleanCLIHome, w.leaver)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, t)
	}
	return out
}

func (w *world) history(rows ...string) {
	w.t.Helper()
	exec(w.t, w.db, `DELETE FROM cli_worker_history`)
	for _, r := range rows {
		exec(w.t, w.db, `INSERT INTO cli_worker_history (worker, since) VALUES (?, '2026-01-01T00:00:00.000Z')`, r)
	}
}

func (w *world) localHome(user int64) string {
	return filepath.Join(w.dir, "cli", fmt.Sprintf("u%d", user))
}

func (w *world) makeLocalHome(user int64) {
	w.t.Helper()
	home := filepath.Join(w.localHome(user), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "login"), []byte("a login"), 0o600); err != nil {
		w.t.Fatal(err)
	}
}

func TestTheContainersCopyWaitsForTheContainerWorker(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.history(cliLocal, cliContainer)
	w.makeLocalHome(w.leaver)
	w.makeLocalHome(w.carol)
	w.cli.kind = ""
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.localHome(w.leaver)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("their local CLI directory remains: %v", err)
	}
	if _, err := os.Stat(w.localHome(w.carol)); err != nil {
		t.Errorf("someone else's local CLI directory: %v", err)
	}
	want := []string{fmt.Sprintf("container:%d", w.leaver)}
	if got := w.cliSteps(); !slices.Equal(got, want) {
		t.Fatalf("steps with the workers off = %v, want %v", got, want)
	}

	w.cli.kind = cliLocal
	if err := w.svc.RunCleanups(ctx); err != nil {
		t.Fatal(err)
	}
	if got := w.cliSteps(); !slices.Equal(got, want) {
		t.Fatalf("steps after a run in local mode = %v, want %v: the container's copy is still there", got, want)
	}

	w.cli.kind = cliContainer
	if err := w.svc.RunCleanups(ctx); err != nil {
		t.Fatal(err)
	}
	if got := w.cliSteps(); len(got) != 0 {
		t.Fatalf("steps after the container worker ran = %v", got)
	}
	if !slices.Equal(w.cli.users, []int64{w.leaver}) {
		t.Errorf("the container worker forgot %v", w.cli.users)
	}
}

func TestAFailingContainerWorkerKeepsItsStep(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.history(cliContainer)
	w.cli.fail = errors.New("the worker is down")
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err == nil {
		t.Fatal("the purge went on over a failed worker")
	}
	if got := w.cliSteps(); !slices.Equal(got, []string{fmt.Sprintf("container:%d", w.leaver)}) {
		t.Fatalf("steps = %v", got)
	}
}

func TestLocalModeForgetsThroughTheLocalWorker(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.history(cliLocal)
	w.cli.kind = cliLocal
	w.makeLocalHome(w.leaver)
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.cli.users, []int64{w.leaver}) {
		t.Errorf("the local worker forgot %v", w.cli.users)
	}
	if _, err := os.Stat(w.localHome(w.leaver)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("their local CLI directory remains: %v", err)
	}
	if got := w.cliSteps(); len(got) != 0 {
		t.Fatalf("steps = %v", got)
	}
}

func TestNoCLIStepWaitsWhereNoWorkerEverRan(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.history()
	w.cli.kind = ""
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if got := w.cliSteps(); len(got) != 0 {
		t.Fatalf("CLI steps waiting on an install that never had CLI workers: %v", got)
	}
	var pending int
	if err := w.db.QueryRow(`SELECT details ->> '$.cleanups_pending' FROM audit_events WHERE action = 'account_purged' LIMIT 1`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if got := pending; got != 1 {
		t.Errorf("cleanups_pending = %v, want 1 (the providers file only)", got)
	}
}

func TestAnUnknownPastKeepsBothStepsForTheAccountsOfThatPast(t *testing.T) {
	ctx := context.Background()
	t.Run("an account from before the record", func(t *testing.T) {
		w := newWorld(t)
		w.history()
		exec(t, w.db, `UPDATE users SET created_at = '2025-06-01T00:00:00.000Z' WHERE id = ?`, w.leaver)
		exec(t, w.db, `INSERT INTO cli_worker_history (worker, since) VALUES ('unknown', '2026-01-01T00:00:00.000Z')`)
		w.cli.kind = ""
		w.deleteAccount()
		if err := w.svc.Purge(ctx, w.leaver); err != nil {
			t.Fatal(err)
		}
		want := []string{fmt.Sprintf("container:%d", w.leaver)}
		if got := w.cliSteps(); !slices.Equal(got, want) {
			t.Fatalf("steps = %v, want %v", got, want)
		}
	})
	t.Run("an account made since", func(t *testing.T) {
		w := newWorld(t)
		w.history()
		exec(t, w.db, `UPDATE users SET created_at = '2026-03-01T00:00:00.000Z' WHERE id = ?`, w.leaver)
		exec(t, w.db, `INSERT INTO cli_worker_history (worker, since) VALUES ('unknown', '2026-01-01T00:00:00.000Z')`)
		w.cli.kind = ""
		w.deleteAccount()
		if err := w.svc.Purge(ctx, w.leaver); err != nil {
			t.Fatal(err)
		}
		if got := w.cliSteps(); len(got) != 0 {
			t.Fatalf("steps = %v, want none", got)
		}
	})
}

func TestRunCleanupsLogsWhatTheWaitingStepsWaitFor(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.history(cliContainer)
	w.cli.kind = ""
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if err := w.svc.RunCleanups(ctx); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "waits_for=cli_worker_container accounts=1 steps=1") {
		t.Errorf("log = %q", got)
	}
}

func TestAnUnreachableContainerWorkerDoesNotKeepTheLocalDirectory(t *testing.T) {
	ctx := context.Background()
	for _, history := range [][]string{{cliLocal, cliContainer}, {cliUnknown}} {
		t.Run(fmt.Sprint(history), func(t *testing.T) {
			w := newWorld(t)
			w.history(history...)
			exec(t, w.db, `UPDATE users SET created_at = '2025-06-01T00:00:00.000Z' WHERE id = ?`, w.leaver)
			w.makeLocalHome(w.leaver)
			w.cli.kind = cliContainer
			w.cli.fail = errors.New("the worker's socket doesn't answer")
			w.deleteAccount()
			if err := w.svc.Purge(ctx, w.leaver); err == nil {
				t.Fatal("the purge went on over an unreachable worker")
			}
			if _, err := os.Stat(w.localHome(w.leaver)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("their local CLI directory remains: %v", err)
			}
			if got := w.cliSteps(); !slices.Equal(got, []string{fmt.Sprintf("container:%d", w.leaver)}) {
				t.Fatalf("steps = %v, want only the container's", got)
			}
			w.cli.fail = nil
			if err := w.svc.Purge(ctx, w.leaver); err != nil {
				t.Fatal(err)
			}
			if got := w.cliSteps(); len(got) != 0 {
				t.Fatalf("steps after the worker answered = %v", got)
			}
		})
	}
}

func TestTheLocalCLIStepWaitsForTheBrowserStep(t *testing.T) {
	ctx := context.Background()
	w, b, _ := browserWorld(t)
	w.history(cliLocal)
	w.svc.o.CLI.(*orderedCLI).kind = cliLocal
	b.start(t, w.leaver)
	b.fail = errors.New("not yet")
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err == nil {
		t.Fatal("the purge went on over a browser that couldn't be stopped")
	}
	if _, err := os.Stat(w.localHome(w.leaver)); err != nil {
		t.Fatalf("the local CLI directory went while the browser step was recorded: %v", err)
	}
	b.fail = nil
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.localHome(w.leaver)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("their local CLI directory remains: %v", err)
	}
	if got := w.cliSteps(); len(got) != 0 {
		t.Fatalf("steps = %v", got)
	}
}
