package access

import (
	"context"
	"path/filepath"
	"testing"

	"taskto.ai/taskto/internal/db"
)

func TestSpaceRowCheckTellsTheSameSpaceFromOneThatTookItsID(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO workspaces (id, name) VALUES (1, 'One'), (2, 'Two')`); err != nil {
		t.Fatal(err)
	}
	insert := func(created string) {
		t.Helper()
		if _, err := sqlDB.Exec(`INSERT INTO spaces (id, workspace_id, kind, name, created_at) VALUES (7, 1, 'shared', 'S', ?)`, created); err != nil {
			t.Fatal(err)
		}
	}
	exists := SpaceRowCheck(sqlDB)
	check := func(s Space, want bool, why string) {
		t.Helper()
		got, err := exists(s)
		if err != nil || got != want {
			t.Fatalf("%s: = %v, %v; want %v", why, got, err, want)
		}
	}

	old := Space{ID: 7, WorkspaceID: 1, CreatedAt: "2026-01-01T00:00:00.000Z"}
	check(old, false, "no row")
	insert(old.CreatedAt)
	check(old, true, "its row")
	check(Space{ID: 7, WorkspaceID: 1}, true, "a space read without its creation time is told by its IDs")
	check(Space{ID: 7, WorkspaceID: 2, CreatedAt: old.CreatedAt}, false, "another workspace's space of the same ID")
	check(Space{ID: 8, WorkspaceID: 1, CreatedAt: old.CreatedAt}, false, "another ID")

	if _, err := sqlDB.Exec(`DELETE FROM spaces WHERE id = 7`); err != nil {
		t.Fatal(err)
	}
	insert("2026-02-01T00:00:00.000Z")
	check(old, false, "the space that was, once another has its ID")
	check(Space{ID: 7, WorkspaceID: 1, CreatedAt: "2026-02-01T00:00:00.000Z"}, true, "the one that has it")
}
