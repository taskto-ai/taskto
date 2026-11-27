package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

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

func TestCLIWorkerHistoryOfAnOlderInstallIsUnknown(t *testing.T) {
	for _, from := range []int{53, 58} {
		t.Run(fmt.Sprintf("from %04d", from), func(t *testing.T) {
			ctx := context.Background()
			sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
			if _, err := migrate(ctx, sqlDB, upTo(t, from)); err != nil {
				t.Fatal(err)
			}
			queries := []string{
				`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x')`,
				`INSERT INTO users (id, email, name, password_hash, created_at) VALUES (2, 'b@example.com', 'B', 'x', '2999-01-01T00:00:00.000Z')`,
				`INSERT INTO purge_cleanups (user_id, kind, target, attempts, last_error) VALUES (7, 'cli_home', '7', 3, 'cli_worker_failed')`,
				`INSERT INTO purge_cleanups (user_id, kind, target) VALUES (7, 'providers', 'feed:7')`,
			}
			if from >= 58 {
				queries = append(queries, `INSERT INTO purge_cleanups (user_id, kind, target) VALUES (7, 'browser', '7')`)
			}
			for _, q := range queries {
				if _, err := sqlDB.ExecContext(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Migrate(ctx, sqlDB); err != nil {
				t.Fatal(err)
			}
			if got := textColumn(t, sqlDB, `SELECT worker FROM cli_worker_history`); !slices.Equal(got, []string{"unknown"}) {
				t.Fatalf("history = %v, want [unknown]", got)
			}
			var since string
			if err := sqlDB.QueryRowContext(ctx, `SELECT since FROM cli_worker_history`).Scan(&since); err != nil {
				t.Fatal(err)
			}
			if since < "2999-01-01T00:00:00.000Z" {
				t.Errorf("unknown since %s: before an account the database has", since)
			}
			got := textColumn(t, sqlDB, `SELECT kind || ' ' || target || ' ' || attempts || ' ' || coalesce(last_error, '')
				FROM purge_cleanups ORDER BY kind, target`)
			want := []string{
				"browser 7 0 ",
				"cli_home container:7 3 cli_worker_failed", "cli_home local:7 3 cli_worker_failed",
				"providers feed:7 0 ",
			}
			if !slices.Equal(got, want) {
				t.Fatalf("steps = %q, want %q", got, want)
			}
		})
	}
}

func textColumn(t *testing.T, sqlDB *sql.DB, query string) []string {
	t.Helper()
	rows, err := sqlDB.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
