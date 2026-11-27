package cliaccounts

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"taskto.ai/taskto/internal/db"
)

func TestRecordWorkerNotesEachKindOnce(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	kinds := func() []string {
		rows, err := sqlDB.QueryContext(ctx, `SELECT worker FROM cli_worker_history ORDER BY worker`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				t.Fatal(err)
			}
			out = append(out, k)
		}
		return out
	}
	if err := RecordWorker(ctx, sqlDB, Off); err != nil {
		t.Fatal(err)
	}
	if got := kinds(); len(got) != 0 {
		t.Fatalf("off recorded %v", got)
	}
	for _, m := range []Mode{LocalUnsafe, Local, Container, Container} {
		if err := RecordWorker(ctx, sqlDB, m); err != nil {
			t.Fatal(err)
		}
	}
	if got := kinds(); !slices.Equal(got, []string{"container", "local"}) {
		t.Fatalf("recorded %v", got)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE cli_worker_history SET since = '2026-01-01T00:00:00.000Z'`); err != nil {
		t.Fatal(err)
	}
	if err := RecordWorker(ctx, sqlDB, Local); err != nil {
		t.Fatal(err)
	}
	var since string
	if err := sqlDB.QueryRowContext(ctx, `SELECT since FROM cli_worker_history WHERE worker = 'local'`).Scan(&since); err != nil {
		t.Fatal(err)
	}
	if since != "2026-01-01T00:00:00.000Z" {
		t.Errorf("a later start moved the first one's time to %s", since)
	}
}
