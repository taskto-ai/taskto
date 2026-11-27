package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestConversationOriginIsImmutableAndConsistent(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x')`,
		`INSERT INTO workspaces (id, name) VALUES (1, 'W')`,
		`INSERT INTO conversations (id, workspace_id, owner_id, agent_id, origin, origin_kind) VALUES (1, 1, 1, 1, 'effect:1', 'app')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE conversations SET origin = 'effect:2' WHERE id = 1`); err == nil {
		t.Fatal("a conversation origin was changed")
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO conversations (id, workspace_id, owner_id, agent_id, origin, origin_kind) VALUES (2, 1, 1, 2, 'effect:1', 'app')`); err == nil {
		t.Fatal("an origin was used twice")
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO conversations (id, workspace_id, owner_id, agent_id, origin) VALUES (2, 1, 1, 2, 'effect:2')`); err == nil {
		t.Fatal("an origin without a kind was admitted")
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO conversations (id, workspace_id, owner_id, agent_id, origin, origin_kind) VALUES (2, 1, 1, 2, NULL, 'app')`); err == nil {
		t.Fatal("a kind without an origin was admitted")
	}
}
