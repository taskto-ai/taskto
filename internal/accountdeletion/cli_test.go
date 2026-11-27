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

	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/cliworker"
)

// cliSteps lists the CLI steps recorded for the leaver, sorted.
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

// Both kinds of worker have run here. The purge runs with the workers off: the local worker's
// directory is removed from the data directory, and the container worker's copy waits. The operator
// then turns on local mode: the local worker can't reach the container's volume, so the step stays.
// Only the container worker, once it runs, clears it.
func TestTheContainersCopyWaitsForTheContainerWorker(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.history(cliLocal, cliContainer)
	w.makeLocalHome(w.leaver)
	w.makeLocalHome(w.carol)
	w.cli.kind = "" // CLI workers off
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

	// Local mode: its Forget succeeds, but it isn't the worker that holds the copy.
	w.cli.kind = cliLocal
	if err := w.svc.RunCleanups(ctx); err != nil {
		t.Fatal(err)
	}
	if got := w.cliSteps(); !slices.Equal(got, want) {
		t.Fatalf("steps after a run in local mode = %v, want %v: the container's copy is still there", got, want)
	}

	// The container worker runs: it removes its copy, and the step is done.
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

// A container worker that fails keeps the step, as a failure.
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

// In local mode the local worker forgets the person (stopping their logins) and the directory goes.
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

// An install that never ran a CLI worker has nothing of the person's to clean: the purge completes
// with no CLI step left waiting, and says so.
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

// Where the record doesn't go back far enough (an install from before it, or a restore), an account
// that existed then may have files with either worker: both steps wait, whatever runs now. An account
// made since has files only with a worker that ran since, which the record has.
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
		// The local copy is removed from here; the container's waits.
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

// The log says what waiting steps wait for, and for how many accounts.
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

// The container worker doesn't answer: its step fails and stays, but the local directory under the
// data directory is removed all the same (each kind is a step of its own), and the account's purge
// is tried again until the container's copy is gone too.
func TestAnUnreachableContainerWorkerDoesNotKeepTheLocalDirectory(t *testing.T) {
	ctx := context.Background()
	for _, history := range [][]string{{cliLocal, cliContainer}, {cliUnknown}} {
		t.Run(fmt.Sprint(history), func(t *testing.T) {
			w := newWorld(t)
			w.history(history...)
			// An account from before the history was known, for the unknown case.
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

// The local CLI directory, like the container's copy, holds the browser's profile: its step waits
// while the browser step is recorded.
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

// An account that used the CLIs in local mode has its files in a home of its own
// (<data>/cli/h<home>), found through the local worker's record of its identity. Deleted while
// no local worker runs (the workers off, or in container mode now), the purge still removes that
// home, and retires the account so a local worker started later refuses it; another account's
// home stays.
func TestThePurgeRemovesALocalAccountHomeWithTheWorkersOff(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.history(cliLocal)
	layout := cliworker.Layout{Root: filepath.Join(w.dir, "cli")}
	worker, err := cliworker.New(cliworker.Config{Layout: layout})
	if err != nil {
		t.Fatal(err)
	}
	homes, accounts := map[int64]string{}, map[int64]cliowner.Account{}
	for _, user := range []int64{w.leaver, w.carol} {
		var identity string
		if err := w.db.QueryRow(`SELECT cli_identity FROM users WHERE id = ?`, user).Scan(&identity); err != nil {
			t.Fatal(err)
		}
		accounts[user] = cliowner.Account{User: user, Identity: identity}
		dirs, err := worker.AccountDirs(accounts[user], "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dirs.Home, "login"), []byte("a login"), 0o600); err != nil {
			t.Fatal(err)
		}
		homes[user] = dirs.Root
	}
	worker.Close()
	w.cli.kind = "" // CLI workers off
	w.deleteAccount()
	if err := w.svc.Purge(ctx, w.leaver); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(homes[w.leaver]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("their local CLI home remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homes[w.carol], "home", "login")); err != nil {
		t.Errorf("someone else's local CLI home: %v", err)
	}
	if got := w.cliSteps(); len(got) != 0 {
		t.Errorf("steps left = %v", got)
	}
	// A local worker started later refuses the deleted account, and makes it no home.
	again, err := cliworker.New(cliworker.Config{Layout: layout})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, err := again.AccountDirs(accounts[w.leaver], "", ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("the deleted account on a local worker started later: %v, want ErrForgotten", err)
	}
}
