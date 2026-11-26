package db

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

// A new install has no history of CLI workers: no worker ever ran, so a purge has nothing of
// theirs to wait for.
func TestCLIWorkerHistoryIsEmptyOnANewInstall(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM cli_worker_history`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d rows of history on a new install", n)
	}
}

// An install from before the history was kept may have run either worker for its accounts: the
// upgrade says so (unknown), and a purge step recorded then waits for both.
func TestCLIWorkerHistoryOfAnOlderInstallIsUnknown(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, upTo(t, 56)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x')`,
		// made, by a clock ahead of this one, after the upgrade's now
		`INSERT INTO users (id, email, name, password_hash, created_at) VALUES (2, 'b@example.com', 'B', 'x', '2999-01-01T00:00:00.000Z')`,
		`INSERT INTO purge_cleanups (user_id, kind, target, attempts, last_error) VALUES (7, 'cli_home', '7', 3, 'cli_worker_failed')`,
		`INSERT INTO purge_cleanups (user_id, kind, target) VALUES (7, 'providers', 'feed:7')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	var workers []string
	rows, err := sqlDB.QueryContext(ctx, `SELECT worker FROM cli_worker_history`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			t.Fatal(err)
		}
		workers = append(workers, w)
	}
	_ = rows.Close()
	if !slices.Equal(workers, []string{"unknown"}) {
		t.Fatalf("history = %v, want [unknown]", workers)
	}
	var since string
	if err := sqlDB.QueryRowContext(ctx, `SELECT since FROM cli_worker_history`).Scan(&since); err != nil {
		t.Fatal(err)
	}
	if since < "2999-01-01T00:00:00.000Z" {
		t.Errorf("unknown since %s: before an account the database has", since)
	}
	var steps []string
	rows, err = sqlDB.QueryContext(ctx, `SELECT kind || ' ' || target || ' ' || attempts || ' ' || coalesce(last_error, '') FROM purge_cleanups ORDER BY kind, target`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, s)
	}
	_ = rows.Close()
	want := []string{"cli_home container:7 3 cli_worker_failed", "cli_home local:7 3 cli_worker_failed", "providers feed:7 0 "}
	if !slices.Equal(steps, want) {
		t.Fatalf("steps = %q, want %q", steps, want)
	}
}
