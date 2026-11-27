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
	insert := func() int64 {
		t.Helper()
		if _, err := sqlDB.Exec(`INSERT INTO spaces (id, workspace_id, kind, name, created_at) VALUES (7, 1, 'shared', 'S', '2026-01-01T00:00:00.000Z')`); err != nil {
			t.Fatal(err)
		}
		var g int64
		if err := sqlDB.QueryRow(`SELECT generation FROM spaces WHERE id = 7`).Scan(&g); err != nil || g == 0 {
			t.Fatalf("the new space's generation: %d, %v", g, err)
		}
		return g
	}
	exists := SpaceRowCheck(sqlDB)
	check := func(s Space, want bool, why string) {
		t.Helper()
		got, err := exists(s)
		if err != nil || got != want {
			t.Fatalf("%s: = %v, %v; want %v", why, got, err, want)
		}
	}

	check(Space{ID: 7, WorkspaceID: 1, Generation: 1}, false, "no row")
	first := insert()
	old := Space{ID: 7, WorkspaceID: 1, Generation: first}
	check(old, true, "its row")
	check(Space{ID: 7, WorkspaceID: 1}, true, "a space read without its generation is told by its IDs")
	check(Space{ID: 7, WorkspaceID: 2, Generation: first}, false, "another workspace's space of the same ID")
	check(Space{ID: 8, WorkspaceID: 1, Generation: first}, false, "another ID")

	if _, err := sqlDB.Exec(`DELETE FROM spaces WHERE id = 7`); err != nil {
		t.Fatal(err)
	}
	second := insert()
	if second == first {
		t.Fatalf("the generation %d was given twice", first)
	}
	check(old, false, "the space that was, once another has its ID")
	check(Space{ID: 7, WorkspaceID: 1, Generation: second}, true, "the one that has it")
}
