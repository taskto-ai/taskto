package db

import (
	"context"
	"path/filepath"
	"testing"
)

// A database from before generations: the spaces it has get their IDs as generations, the rows
// that refer to them (ON DELETE CASCADE) are all still there, the counter starts at the largest, and
// a space made after takes a number no earlier one had, even in the ID of one deleted.
func TestSpaceGenerationMigrationKeepsEverythingAndNeverRepeats(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, migrationsBefore(t, "_space_generation.sql")); err != nil {
		t.Fatal(err)
	}
	var one int
	if err := sqlDB.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("fixture: foreign keys = %d, %v", one, err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x'), (2, 'b@example.com', 'B', 'x')`,
		`INSERT INTO workspaces (id, name) VALUES (1, 'One'), (2, 'Two')`,
		`INSERT INTO memberships (workspace_id, user_id, role) VALUES (1, 1, 'admin'), (1, 2, 'member'), (2, 1, 'admin')`,
		`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (40, 1, 'shared', 'Team'), (41, 2, 'shared', 'Other')`,
		`INSERT INTO space_members (space_id, user_id, role) VALUES (40, 1, 'editor'), (40, 2, 'viewer'), (41, 1, 'editor')`,
		`INSERT INTO boards (space_id, kind, name, created_by) VALUES (40, 'team', 'Work', 1), (41, 'team', 'Work', 1)`,
		`INSERT INTO cards (board_id, title, col, rank) SELECT id, 'Ship it', 'todo', 'a' FROM boards`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := map[string]int{}
	tables := []string{"spaces", "space_members", "boards", "cards", "memberships", "workspaces"}
	for _, tb := range tables {
		before[tb] = count(tb)
	}

	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	for _, tb := range tables {
		if n := count(tb); n != before[tb] {
			t.Fatalf("%s had %d rows before the migration and %d after", tb, before[tb], n)
		}
	}
	var wrong int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM spaces WHERE generation <> id`).Scan(&wrong); err != nil || wrong != 0 {
		t.Fatalf("%d spaces whose generation isn't their ID (%v)", wrong, err)
	}
	var largest, last int64
	if err := sqlDB.QueryRowContext(ctx, `SELECT max(generation) FROM spaces`).Scan(&largest); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.QueryRowContext(ctx, `SELECT last FROM space_generations`).Scan(&last); err != nil || last != largest {
		t.Fatalf("the counter starts at %d (%v), the largest generation is %d", last, err, largest)
	}

	// A new space is above every one there was, in the same transaction as its insert.
	insert := func() (id, generation int64) {
		t.Helper()
		res, err := sqlDB.ExecContext(ctx, `INSERT INTO spaces (workspace_id, kind, name) VALUES (1, 'shared', 'New')`)
		if err != nil {
			t.Fatal(err)
		}
		if id, err = res.LastInsertId(); err != nil {
			t.Fatal(err)
		}
		if err := sqlDB.QueryRowContext(ctx, `SELECT generation FROM spaces WHERE id = ?`, id).Scan(&generation); err != nil {
			t.Fatal(err)
		}
		return id, generation
	}
	id1, gen1 := insert()
	if gen1 <= largest {
		t.Fatalf("a new space's generation %d is not above %d", gen1, largest)
	}
	// Deleted, and its ID is the largest again: SQLite gives it to the next space, which gets a
	// generation of its own.
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM spaces WHERE id = ?`, id1); err != nil {
		t.Fatal(err)
	}
	id2, gen2 := insert()
	if id2 != id1 {
		t.Fatalf("fixture: the ID was not reused (%d, then %d)", id1, id2)
	}
	if gen2 <= gen1 {
		t.Fatalf("the space that took ID %d has generation %d, no more than the one before's %d", id1, gen2, gen1)
	}
	if err := sqlDB.QueryRowContext(ctx, `SELECT last FROM space_generations`).Scan(&last); err != nil || last != gen2 {
		t.Fatalf("the counter is %d (%v), want %d", last, err, gen2)
	}

	// Never changed, never taken from the caller, the counter never lowered or removed.
	for _, q := range []string{
		`UPDATE spaces SET generation = generation + 1 WHERE id = 40`,
		`UPDATE spaces SET generation = 0 WHERE id = 40`,
		`INSERT INTO spaces (workspace_id, kind, name, generation) VALUES (1, 'shared', 'Chosen', 999)`,
		`UPDATE space_generations SET last = 1`,
		`DELETE FROM space_generations`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err == nil {
			t.Fatalf("%s was accepted", q)
		}
	}
	if n := count("spaces"); n != before["spaces"]+1 { // the one made last
		t.Fatalf("spaces = %d after the refused statements", n)
	}
}
