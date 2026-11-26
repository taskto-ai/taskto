package main

import (
	"context"
	"path/filepath"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/db"
)

// The knowledge base's check of a space's row finds the space the request was authorized for, and
// not one that took its ID after the space was purged.
func TestSpaceRowExistsIdentifiesTheSameSpace(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO workspaces (id, name) VALUES (1, 'Home'), (2, 'Other')`); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO spaces (id, workspace_id, kind, name, created_at) VALUES (5, 1, 'shared', 'Team', '2026-01-01T00:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	check := spaceRowExists(sqlDB)
	for _, c := range []struct {
		name string
		sp   access.Space
		want bool
	}{
		{"the space as it was read", access.Space{ID: 5, WorkspaceID: 1, CreatedAt: "2026-01-01T00:00:00.000Z"}, true},
		{"no creation time: by IDs", access.Space{ID: 5, WorkspaceID: 1}, true},
		{"a space that took the ID later", access.Space{ID: 5, WorkspaceID: 1, CreatedAt: "2025-12-31T00:00:00.000Z"}, false},
		{"another workspace's same ID", access.Space{ID: 5, WorkspaceID: 2}, false},
		{"no such space", access.Space{ID: 6, WorkspaceID: 1}, false},
	} {
		got, err := check(c.sp)
		if err != nil || got != c.want {
			t.Errorf("%s: %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}
