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

// The index's documents are numbered so that no ID is given twice. A database that has documents,
// their full text rows and their chunks keeps all of them, with the same IDs, and a document made
// after the newest was deleted takes a new ID.
func TestSearchDocumentIDsAreNeverReusedAndTheMigrationKeepsTheIndex(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, migrationsBefore(t, "_space_generation.sql")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO workspaces (id, name) VALUES (1, 'One')`,
		`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (5, 1, 'shared', 'Team'), (6, 1, 'shared', 'Other')`,
		`INSERT INTO search_docs (id, space_id, path, version, markdown, title, embed_version, embed_key, embed_tries, text_format)
			VALUES (11, 5, 'a.md', 'v1', 1, 'A', 'v1', 'k', 0, 3), (12, 5, 'b.md', 'v2', 1, 'B', NULL, NULL, 2, 3), (20, 6, 'c.md', 'v3', 1, 'C', 'v3', 'k', 0, 3)`,
		`INSERT INTO search_fts (rowid, path, title, body) VALUES (11, 'a.md', 'A', 'kumquat'), (12, 'b.md', 'B', 'quince'), (20, 'c.md', 'C', 'loquat')`,
		`INSERT INTO search_chunks (doc_id, seq, text, vector) VALUES (11, 0, 'kumquat', x'0102'), (11, 1, 'more', x'0304'), (20, 0, 'loquat', x'0506')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	str := func(q string) string {
		t.Helper()
		var s string
		if err := sqlDB.QueryRowContext(ctx, q).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s
	}
	if got := str(`SELECT group_concat(id || ':' || space_id || ':' || path || ':' || version || ':' || coalesce(embed_version, '-') || ':' || embed_tries || ':' || text_format, ' ') FROM (SELECT * FROM search_docs ORDER BY id)`); got != "11:5:a.md:v1:v1:0:3 12:5:b.md:v2:-:2:3 20:6:c.md:v3:v3:0:3" {
		t.Fatalf("documents after the migration: %s", got)
	}
	if got := str(`SELECT group_concat(rowid || body, ' ') FROM (SELECT rowid, body FROM search_fts ORDER BY rowid)`); got != "11kumquat 12quince 20loquat" {
		t.Fatalf("full text rows after the migration: %s", got)
	}
	if got := str(`SELECT group_concat(doc_id || '/' || seq || ':' || text || ':' || hex(vector), ' ') FROM (SELECT * FROM search_chunks ORDER BY doc_id, seq)`); got != "11/0:kumquat:0102 11/1:more:0304 20/0:loquat:0506" {
		t.Fatalf("chunks after the migration: %s", got)
	}

	// The newest document is deleted with its space's rows, and another is made: a new ID.
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM spaces WHERE id = 6`); err != nil {
		t.Fatal(err)
	}
	if got := str(`SELECT count(*) || ':' || (SELECT count(*) FROM search_fts WHERE rowid = 20) || ':' || (SELECT count(*) FROM search_chunks WHERE doc_id = 20) FROM search_docs WHERE id = 20`); got != "0:0:0" {
		t.Fatalf("what is left of the deleted space's document, its full text row and chunks: %s", got)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO spaces (id, workspace_id, kind, name) VALUES (6, 1, 'shared', 'Replacement')`); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO search_docs (space_id, path, version, markdown, title) VALUES (6, 'c.md', 'v9', 1, 'C') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id <= 20 {
		t.Fatalf("the document made after the newest was deleted has ID %d, which an earlier document had", id)
	}
}
