package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func open(t *testing.T, path string) *sql.DB {
	t.Helper()
	sqlDB, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

func TestOpenCreatesTheFileWithPragmas(t *testing.T) {
	// A '?' and '#' in the path must not be taken for the DSN's query or fragment.
	dir := filepath.Join(t.TempDir(), "odd ?#dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, FileName)
	sqlDB := open(t, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file: %v", err)
	}

	// Several connections, since the pool opens more than one and each needs the pragmas.
	sqlDB.SetMaxIdleConns(3)
	ctx := context.Background()
	var conns []*sql.Conn
	for range 3 {
		conn, err := sqlDB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
	}
	for i, conn := range conns {
		for pragma, want := range map[string]string{
			"journal_mode": "wal",
			"foreign_keys": "1",
			"busy_timeout": "5000",
			"synchronous":  "1", // NORMAL
		} {
			var got string
			if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("connection %d: PRAGMA %s = %s, want %s", i, pragma, got, want)
			}
		}
		_ = conn.Close()
	}
}

func TestOpenFailsOnAnUnusablePath(t *testing.T) {
	_, err := Open(context.Background(), filepath.Join(t.TempDir(), "missing", "dir", FileName))
	if err == nil {
		t.Fatal("Open succeeded in a directory that doesn't exist")
	}
}

func tables(t *testing.T, sqlDB *sql.DB) []string {
	t.Helper()
	rows, err := sqlDB.Query(`SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

func recorded(t *testing.T, sqlDB *sql.DB) []string {
	t.Helper()
	rows, err := sqlDB.Query(`SELECT name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

func TestEmbeddedMigrationsOnAFreshDatabaseThenAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	sqlDB := open(t, path)

	applied, err := Migrate(context.Background(), sqlDB)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) == 0 || applied[0] != "0001_instance.sql" {
		t.Fatalf("applied = %v, want every migration from 0001_instance.sql", applied)
	}
	if got := recorded(t, sqlDB); !slices.Equal(got, applied) {
		t.Fatalf("schema_migrations = %v, want %v", got, applied)
	}

	// A re-run, here after reopening the file as a restart would, changes nothing.
	_ = sqlDB.Close()
	sqlDB = open(t, path)
	before := tables(t, sqlDB)
	again, err := Migrate(context.Background(), sqlDB)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second run applied %v, want nothing", again)
	}
	if got := recorded(t, sqlDB); !slices.Equal(got, applied) {
		t.Fatalf("schema_migrations after a re-run = %v, want %v", got, applied)
	}
	if after := tables(t, sqlDB); !slices.Equal(after, before) {
		t.Fatalf("tables changed on a re-run: %v → %v", before, after)
	}
}

func TestMigrationsRunInNumberOrder(t *testing.T) {
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	fsys := fstest.MapFS{
		// 0002 needs the table 0001 makes; listed out of order on purpose.
		"0002_b.sql":  {Data: []byte(`INSERT INTO a (x) VALUES (1);`)},
		"0001_a.sql":  {Data: []byte(`CREATE TABLE a (x INTEGER);`)},
		"0010_ab.sql": {Data: []byte(`INSERT INTO a (x) VALUES (10);`)},
	}
	applied, err := migrate(context.Background(), sqlDB, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"0001_a.sql", "0002_b.sql", "0010_ab.sql"}; !slices.Equal(applied, want) {
		t.Fatalf("applied = %v, want %v", applied, want)
	}

	// A new migration is applied alone on the next run.
	fsys["0011_c.sql"] = &fstest.MapFile{Data: []byte(`INSERT INTO a (x) VALUES (11);`)}
	applied, err = migrate(context.Background(), sqlDB, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"0011_c.sql"}; !slices.Equal(applied, want) {
		t.Fatalf("applied = %v, want %v", applied, want)
	}
	var n int
	if err := sqlDB.QueryRow(`SELECT count(*) FROM a`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("rows in a = %d (%v), want 3", n, err)
	}
}

func TestAFailingMigrationRollsBackAndStops(t *testing.T) {
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	fsys := fstest.MapFS{
		"0001_ok.sql": {Data: []byte(`CREATE TABLE ok (x INTEGER);`)},
		// The first statement works; the second fails, so the first must be undone.
		"0002_bad.sql":   {Data: []byte("CREATE TABLE half (x INTEGER);\nINSERT INTO ok (x) VALUES (1);\nNOT SQL;")},
		"0003_after.sql": {Data: []byte(`CREATE TABLE after (x INTEGER);`)},
	}
	applied, err := migrate(context.Background(), sqlDB, fsys)
	if err == nil || !strings.Contains(err.Error(), "migration 0002_bad.sql") {
		t.Fatalf("err = %v, want the failure of 0002_bad.sql", err)
	}
	if want := []string{"0001_ok.sql"}; !slices.Equal(applied, want) {
		t.Fatalf("applied = %v, want %v", applied, want)
	}
	if got := recorded(t, sqlDB); !slices.Equal(got, []string{"0001_ok.sql"}) {
		t.Fatalf("schema_migrations = %v, want only 0001_ok.sql", got)
	}
	if got := tables(t, sqlDB); !slices.Equal(got, []string{"ok", "schema_migrations"}) {
		t.Fatalf("tables = %v: the failed migration was not rolled back, or a later one ran", got)
	}
	var n int
	if err := sqlDB.QueryRow(`SELECT count(*) FROM ok`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows in ok = %d (%v), want 0", n, err)
	}

	// Once fixed, the next start applies it and the ones after it.
	fsys["0002_bad.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE half (x INTEGER);`)}
	applied, err = migrate(context.Background(), sqlDB, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"0002_bad.sql", "0003_after.sql"}; !slices.Equal(applied, want) {
		t.Fatalf("applied = %v, want %v", applied, want)
	}
}

func TestMigrationFileProblems(t *testing.T) {
	tests := []struct {
		name    string
		fsys    fstest.MapFS
		wantErr string
	}{
		{"bad name", fstest.MapFS{"1_init.sql": {}}, `"1_init.sql" isn't named NNNN_description.sql`},
		{"not sql", fstest.MapFS{"0001_init.txt": {}}, "isn't named"},
		{"same number", fstest.MapFS{"0001_a.sql": {}, "0001_b.sql": {}}, "0001_a.sql and 0001_b.sql have the same number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
			if _, err := migrate(context.Background(), sqlDB, tt.fsys); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestEmbeddedMigrationNumbersAreUnique fails fast, before any database is opened, when two
// pull requests that each took the next free number are both merged: every test that opens
// a database would otherwise fail with the same opaque error. It names every clash, so the
// later-merged file can be renumbered at once.
func TestEmbeddedMigrationNumbersAreUnique(t *testing.T) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	byNumber := map[string][]string{}
	for _, e := range entries {
		match := migrationName.FindStringSubmatch(e.Name())
		if match == nil {
			t.Errorf("%q isn't named NNNN_description.sql", e.Name())
			continue
		}
		byNumber[match[1]] = append(byNumber[match[1]], e.Name())
	}
	for number, names := range byNumber {
		if len(names) > 1 {
			t.Errorf("migrations %s share the number %s: renumber the one merged later to the next free number", strings.Join(names, " and "), number)
		}
	}
}

func TestADatabaseFromANewerVersionIsRefused(t *testing.T) {
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	newer := fstest.MapFS{
		"0001_a.sql": {Data: []byte(`CREATE TABLE a (x INTEGER);`)},
		"0002_b.sql": {Data: []byte(`CREATE TABLE b (x INTEGER);`)},
	}
	if _, err := migrate(context.Background(), sqlDB, newer); err != nil {
		t.Fatal(err)
	}
	older := fstest.MapFS{"0001_a.sql": newer["0001_a.sql"]}
	if _, err := migrate(context.Background(), sqlDB, older); err == nil || !strings.Contains(err.Error(), "0002_b.sql, which this binary doesn't know") {
		t.Fatalf("err = %v, want a refusal naming 0002_b.sql", err)
	}
}

func TestAMigrationChangedAfterItRanIsRefused(t *testing.T) {
	original := fstest.MapFS{"0001_a.sql": {Data: []byte(`CREATE TABLE a (x INTEGER);`)}}
	cases := map[string]fstest.MapFS{
		"edited":  {"0001_a.sql": {Data: []byte(`CREATE TABLE a (x INTEGER, y TEXT);`)}},
		"renamed": {"0001_renamed.sql": original["0001_a.sql"]},
	}
	for name, changed := range cases {
		t.Run(name, func(t *testing.T) {
			sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
			if _, err := migrate(context.Background(), sqlDB, original); err != nil {
				t.Fatal(err)
			}
			if _, err := migrate(context.Background(), sqlDB, changed); err == nil || !strings.Contains(err.Error(), "changed after it was applied") {
				t.Fatalf("err = %v, want a refusal of the changed migration", err)
			}
			// The unchanged set still starts.
			if _, err := migrate(context.Background(), sqlDB, original); err != nil {
				t.Fatalf("unchanged migrations: %v", err)
			}
		})
	}
}

// A reworded migration's comments changed and nothing else: without its comment lines, it
// is still the file the databases ran.
func TestRewordedMigrationsChangedOnlyComments(t *testing.T) {
	for name, r := range reworded {
		raw, err := fs.ReadFile(migrations, "migrations/"+name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var kept []string
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				kept = append(kept, line)
			}
		}
		sum := sha256.Sum256([]byte(strings.Join(kept, "\n")))
		if got := hex.EncodeToString(sum[:]); got != r.sqlOnly {
			t.Errorf("%s: more than its comments changed (SQL checksum %s, want %s): add a new migration instead", name, got, r.sqlOnly)
		}
	}
}

// A database that ran a reworded migration as it was before still opens; any other
// checksum is still refused.
func TestADatabaseWithAMigrationBeforeItsRewordingOpens(t *testing.T) {
	for name, r := range reworded {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			sqlDB := open(t, path)
			if _, err := Migrate(context.Background(), sqlDB); err != nil {
				t.Fatal(err)
			}
			if _, err := sqlDB.Exec(`UPDATE schema_migrations SET checksum = ? WHERE name = ?`, r.before, name); err != nil {
				t.Fatal(err)
			}
			if _, err := Migrate(context.Background(), sqlDB); err != nil {
				t.Fatalf("with the checksum from before the rewording: %v", err)
			}
			if _, err := sqlDB.Exec(`UPDATE schema_migrations SET checksum = 'edited' WHERE name = ?`, name); err != nil {
				t.Fatal(err)
			}
			if _, err := Migrate(context.Background(), sqlDB); err == nil || !strings.Contains(err.Error(), "changed after it was applied") {
				t.Fatalf("err = %v, want a refusal of the changed migration", err)
			}
		})
	}
}

// upTo returns the embedded migrations numbered up to version, as an older binary had
// them.
func upTo(t *testing.T, version int) fstest.MapFS {
	t.Helper()
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	all, err := load(sub)
	if err != nil {
		t.Fatal(err)
	}
	older := fstest.MapFS{}
	for _, m := range all {
		if m.version <= version {
			older[m.name] = &fstest.MapFile{Data: []byte(m.sql)}
		}
	}
	return older
}

// TestSpacesBackfillPrivateSpaces upgrades a database with members from before spaces:
// each membership gets its private space, and members joining later get theirs.
func TestSpacesBackfillPrivateSpaces(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, upTo(t, 2)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x'), (2, 'b@example.com', 'B', 'x'), (3, 'c@example.com', 'C', 'x')`,
		`INSERT INTO workspaces (id, name) VALUES (1, 'One'), (2, 'Two')`,
		`INSERT INTO memberships (workspace_id, user_id, role) VALUES (1, 1, 'admin'), (1, 2, 'member'), (2, 1, 'admin')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (2, 3, 'member')`); err != nil {
		t.Fatal(err)
	}

	rows, err := sqlDB.QueryContext(ctx, `SELECT workspace_id, owner_id FROM spaces WHERE kind = 'private' ORDER BY workspace_id, owner_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var ws, owner int
		if err := rows.Scan(&ws, &owner); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d/%d", ws, owner))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"1/1", "1/2", "2/1", "2/3"}; !slices.Equal(got, want) {
		t.Fatalf("private spaces (workspace/owner) = %v, want %v", got, want)
	}
}

// TestConversationLinksAreIDs upgrades a database with card links from before 0008: links
// that aren't a conversation ID go, the others stay, and new ones must be IDs.
func TestConversationLinksAreIDs(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, upTo(t, 7)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x')`,
		`INSERT INTO workspaces (id, name) VALUES (1, 'One')`,
		`INSERT INTO memberships (workspace_id, user_id, role) VALUES (1, 1, 'admin')`,
		`INSERT INTO cards (id, board_id, title, col, rank) SELECT 1, id, 'Card', 'todo', 'm' FROM boards`,
		`INSERT INTO card_links (card_id, kind, target) VALUES
			(1, 'conversation', '12'), (1, 'conversation', 'conv-1'), (1, 'conversation', '012'),
			(1, 'conversation', ''), (1, 'doc', 'notes/plan.md')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	rows, err := sqlDB.QueryContext(ctx, `SELECT kind || ':' || target FROM card_links ORDER BY kind, target`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatal(err)
		}
		got = append(got, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"conversation:12", "doc:notes/plan.md"}; !slices.Equal(got, want) {
		t.Fatalf("links after the upgrade = %v, want %v", got, want)
	}
	for target, ok := range map[string]bool{"7": true, "9223372036854775807": true, "07": false, "-7": false, "7x": false, "": false, "12345678901234567890": false} {
		_, err := sqlDB.ExecContext(ctx, `INSERT INTO card_links (card_id, kind, target) VALUES (1, 'conversation', ?)`, target)
		if (err == nil) != ok {
			t.Errorf("inserting the link %q: %v", target, err)
		}
	}
}

// TestSearchIndexMigratesToUnspacedScripts runs migration 0011 on a database with a search
// index made before it: the full-text index is emptied, for the server to index again from
// git, with marks kept within words; documents and their embeddings stay.
func TestSearchIndexMigratesToUnspacedScripts(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() < "0011" {
			raw, err := fs.ReadFile(sub, e.Name())
			if err != nil {
				t.Fatal(err)
			}
			before[e.Name()] = &fstest.MapFile{Data: raw}
		}
	}
	if _, err := migrate(ctx, sqlDB, before); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO workspaces (id, name) VALUES (1, 'Home')`,
		`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (1, 1, 'shared', 'Team')`,
		`INSERT INTO search_spaces (space_id, commit_id) VALUES (1, 'c')`,
		`INSERT INTO search_docs (id, space_id, path, version, markdown, title, embed_version, embed_key)
			VALUES (1, 1, 'a.md', 'v', 1, 'A', 'v', 'k')`,
		`INSERT INTO search_fts (rowid, path, title, body) VALUES (1, 'a.md', 'A', '東京都庁')`,
		`INSERT INTO search_chunks (doc_id, seq, text, vector) VALUES (1, 0, '東京都庁', x'0000803f')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	applied, err := Migrate(ctx, sqlDB)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) == 0 || applied[0] != "0011_search_unspaced_scripts.sql" {
		t.Fatalf("applied %v", applied)
	}
	var partial, format, fts, chunks int
	var embedded string
	if err := sqlDB.QueryRowContext(ctx, `SELECT (SELECT partial FROM search_spaces), (SELECT text_format FROM search_docs),
		(SELECT count(*) FROM search_fts), (SELECT count(*) FROM search_chunks), (SELECT embed_version FROM search_docs)`).
		Scan(&partial, &format, &fts, &chunks, &embedded); err != nil {
		t.Fatal(err)
	}
	if partial != 1 || format != 0 || fts != 0 || chunks != 1 || embedded != "v" {
		t.Fatalf("partial %d, text_format %d, %d full-text rows, %d chunks, embedded %q", partial, format, fts, chunks, embedded)
	}

	// Marks stay within words: กิน (eat) isn't ก้น.
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO search_fts (rowid, path, title, body) VALUES (1, '', '', 'ก้น')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM search_fts WHERE search_fts MATCH '"กิน"'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("กิน found ก้น: %d (%v)", n, err)
	}
}

// TestDictationAutoBecomesLocal runs migration 0018 on settings from before it: auto, which
// used the hosted API as soon as a key was set, becomes local; an admin's explicit choice and
// the sealed key stay, and a new row defaults to local.
func TestDictationAutoBecomesLocal(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, upTo(t, 17)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO workspaces (id, name) VALUES (1, 'Auto with a key'), (2, 'API'), (3, 'Add-on'), (4, 'New')`,
		`INSERT INTO dictation_settings (workspace_id, engine, language, api_key) VALUES
			(1, 'auto', 'pt', X'0102'), (2, 'api', '', X'0304')`,
		`INSERT INTO dictation_settings (workspace_id, engine, addon_notice) VALUES (3, 'desertant-voz', 'desertant-voz')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO dictation_settings (workspace_id) VALUES (4)`); err != nil {
		t.Fatal(err)
	}
	rows, err := sqlDB.QueryContext(ctx, `SELECT workspace_id, engine, language, hex(coalesce(api_key, X'')), coalesce(addon_notice, '')
		FROM dictation_settings ORDER BY workspace_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var ws int
		var engine, language, key, notice string
		if err := rows.Scan(&ws, &engine, &language, &key, &notice); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d %s %q %s %q", ws, engine, language, key, notice))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{`1 local "pt" 0102 ""`, `2 api "" 0304 ""`, `3 desertant-voz ""  "desertant-voz"`, `4 local ""  ""`}
	if !slices.Equal(got, want) {
		t.Fatalf("settings after the upgrade = %q, want %q", got, want)
	}
}

// TestAppRolesMigrationAddsNoAssignments upgrades a database that has apps with rows from
// before app roles: no app gets a role assignment, which is what keeps every app as
// unrestricted as it was, and the rows keep the creators that own-record reading needs.
func TestAppRolesMigrationAddsNoAssignments(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, upTo(t, 20)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x'), (2, 'b@example.com', 'B', 'x')`,
		`INSERT INTO workspaces (id, name) VALUES (1, 'One')`,
		`INSERT INTO memberships (workspace_id, user_id, role) VALUES (1, 1, 'admin'), (1, 2, 'member')`,
		`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (10, 1, 'shared', 'Team')`,
		`INSERT INTO space_members (space_id, user_id, role) VALUES (10, 1, 'editor'), (10, 2, 'viewer')`,
		`INSERT INTO apps (id, space_id, name, definition) VALUES (5, 10, 'Tracker', '{}')`,
		`INSERT INTO app_rows (app_id, data, created_by) VALUES (5, '{}', 1), (5, '{}', 2), (5, '{}', NULL)`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	var roles, rows, creators int
	if err := sqlDB.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM app_roles), count(*), count(created_by) FROM app_rows`).
		Scan(&roles, &rows, &creators); err != nil {
		t.Fatal(err)
	}
	if roles != 0 || rows != 3 || creators != 2 {
		t.Fatalf("role assignments %d, rows %d with %d creators; want 0, 3 and 2", roles, rows, creators)
	}
	// A role is one of the five, and held once per user.
	for _, bad := range []string{
		`INSERT INTO app_roles (app_id, user_id, role) VALUES (5, 1, 'owner')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, bad); err == nil {
			t.Fatalf("%s was accepted", bad)
		}
	}
	const assign = `INSERT INTO app_roles (app_id, user_id, role) VALUES (5, NULL, 'reader')`
	if _, err := sqlDB.ExecContext(ctx, assign); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(ctx, assign); err == nil {
		t.Fatal("a role was assigned twice to the same user")
	}
}

// TestAppReleasesMigrationMakesEveryAppRelease1 upgrades a database with apps from before drafts
// and releases: each becomes release 1, live, with its definition as its draft too (and saying
// its format version and what it needs), and no role, row, version or access changes.
func TestAppReleasesMigrationMakesEveryAppRelease1(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, upTo(t, 23)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "apps", "examples", "expenses.json"))
	if err != nil {
		t.Fatal(err)
	}
	var example struct{ Definition json.RawMessage }
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatal(err)
	}
	// A screen that calls a function of the catalog, and one that calls a name it doesn't have.
	called := `{"components":[{"id":"root","component":"Column","children":["when","odd"]},
		{"id":"when","component":"Text","text":{"call":"formatDate","args":{"value":"2024-01-02","format":"yyyy"}}},
		{"id":"odd","component":"Text","text":{"call":"notAFunction","args":{}}}],"actions":{}}`
	for _, q := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x'), (2, 'b@example.com', 'B', 'x')`, nil},
		{`INSERT INTO workspaces (id, name) VALUES (1, 'One')`, nil},
		{`INSERT INTO memberships (workspace_id, user_id, role) VALUES (1, 1, 'admin'), (1, 2, 'member')`, nil},
		{`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (10, 1, 'shared', 'Team')`, nil},
		{`INSERT INTO space_members (space_id, user_id, role) VALUES (10, 1, 'editor'), (10, 2, 'viewer')`, nil},
		{`INSERT INTO apps (id, space_id, name, definition, version, updated_by, archived_at) VALUES (5, 10, 'Tracker', ?, 7, 2, NULL)`, []any{string(example.Definition)}},
		{`INSERT INTO apps (id, space_id, name, definition, archived_at) VALUES (6, 10, 'Calls', ?, '2024-01-01T00:00:00.000Z')`, []any{called}},
		{`INSERT INTO apps (id, space_id, name, definition) VALUES (7, 10, 'Empty', '{}')`, nil},
		{`INSERT INTO app_rows (app_id, data, created_by) VALUES (5, '{"description":"x","amount":1}', 1), (5, '{}', 2)`, nil},
		{`INSERT INTO app_roles (app_id, user_id, role) VALUES (5, 2, 'reader')`, nil},
	} {
		if _, err := sqlDB.ExecContext(ctx, q.q, q.args...); err != nil {
			t.Fatalf("%s: %v", q.q, err)
		}
	}
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}

	type got struct {
		live, differs, version int
		draft, release, reqs   string
		by                     sql.NullInt64
		archived               bool
	}
	read := func(id int) got {
		t.Helper()
		var g got
		if err := sqlDB.QueryRowContext(ctx, `
			SELECT a.live_release, a.draft_differs, a.version, a.definition, r.definition, r.requires, r.published_by, a.archived_at IS NOT NULL
			FROM apps a JOIN app_releases r ON r.app_id = a.id AND r.number = a.live_release WHERE a.id = ?`, id).
			Scan(&g.live, &g.differs, &g.version, &g.draft, &g.release, &g.reqs, &g.by, &g.archived); err != nil {
			t.Fatalf("app %d: %v", id, err)
		}
		return g
	}
	tracker := read(5)
	wantReqs := `["component.Button","component.Column","component.List","component.Row","component.Stat","component.Text","component.TextField"]`
	if tracker.live != 1 || tracker.differs != 0 || tracker.version != 7 || tracker.reqs != wantReqs || tracker.draft != tracker.release ||
		tracker.by.Int64 != 2 || tracker.archived {
		t.Fatalf("tracker = %+v", tracker)
	}
	var v int
	var inDef string
	if err := sqlDB.QueryRowContext(ctx, `SELECT json_extract(definition, '$.definition_version'), json_extract(definition, '$.requires') FROM apps WHERE id = 5`).Scan(&v, &inDef); err != nil || v != 1 || inDef != wantReqs {
		t.Fatalf("the definition says version %d, requires %s (%v)", v, inDef, err)
	}
	// Only the catalog's own functions count; an app taken offline stays so; an empty definition is fine.
	if g := read(6); g.reqs != `["component.Column","component.Text","function.formatDate"]` || !g.archived {
		t.Fatalf("calls = %+v", g)
	}
	if g := read(7); g.reqs != `[]` || g.live != 1 {
		t.Fatalf("empty = %+v", g)
	}
	// No role, row or creator changed.
	var roles, rows, creators int
	if err := sqlDB.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM app_roles), count(*), count(created_by) FROM app_rows`).Scan(&roles, &rows, &creators); err != nil {
		t.Fatal(err)
	}
	if roles != 1 || rows != 2 || creators != 2 {
		t.Fatalf("roles %d, rows %d, creators %d", roles, rows, creators)
	}
	// A release is immutable and numbered per app.
	if _, err := sqlDB.ExecContext(ctx, `UPDATE app_releases SET definition = '{}' WHERE app_id = 5`); err == nil {
		t.Fatal("a release was edited")
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO app_releases (app_id, number, definition, definition_version, requires) VALUES (5, 2, '{}', 1, '[]')`); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE apps SET live_release = 2 WHERE id = 5`); err != nil {
		t.Fatalf("serving release 2: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE apps SET live_release = 3 WHERE id = 5`); err == nil {
		t.Fatal("an app served a release it doesn't have")
	}
	// Its publisher can't be changed, but an account that goes leaves it unattributed
	// (published_by ON DELETE SET NULL).
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (30, 'p@example.com', 'P', 'x')`,
		`INSERT INTO app_releases (app_id, number, definition, definition_version, requires, published_by) VALUES (5, 3, '{}', 1, '[]', 30)`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE app_releases SET published_by = 30 WHERE number = 3`); err == nil {
		t.Fatal("a release's publisher was changed")
	}
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM users WHERE id = 30`); err != nil {
		t.Fatalf("deleting a user who published a release: %v", err)
	}
	var by sql.NullInt64
	if err := sqlDB.QueryRowContext(ctx, `SELECT published_by FROM app_releases WHERE app_id = 5 AND number = 3`).Scan(&by); err != nil || by.Valid {
		t.Fatalf("release's publisher after the user went = %v, %v", by, err)
	}
}

// TestRecordRevisionsMigrationKeepsExistingRows upgrades a database whose apps have rows from
// before revisions: each row is revision 1, live, with one synthetic history entry holding its
// data, credited to its creator and dated at its creation; and history is append-only.
func TestRecordRevisionsMigrationKeepsExistingRows(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, upTo(t, 21)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x')`,
		`INSERT INTO workspaces (id, name) VALUES (1, 'One')`,
		`INSERT INTO memberships (workspace_id, user_id, role) VALUES (1, 1, 'admin')`,
		`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (10, 1, 'shared', 'Team')`,
		`INSERT INTO apps (id, space_id, name, definition) VALUES (5, 10, 'Tracker', '{}')`,
		`INSERT INTO app_rows (id, app_id, data, created_at, created_by, updated_at) VALUES
			(7, 5, '{"title":"Milk"}', '2026-01-02T03:04:05.000Z', 1, '2026-02-03T04:05:06.000Z'),
			(8, 5, '{"title":"Eggs"}', '2026-01-03T03:04:05.000Z', NULL, '2026-01-03T03:04:05.000Z')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	rows, err := sqlDB.QueryContext(ctx, `
		SELECT r.id, r.revision, r.deleted_at IS NULL, h.revision, h.operation, h.data, h.at, h.actor, h.synthetic
		FROM app_rows r JOIN app_row_history h ON h.row_id = r.id ORDER BY r.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, revision, hrevision, synthetic int64
		var live bool
		var op, data, at string
		var actor sql.NullInt64
		if err := rows.Scan(&id, &revision, &live, &hrevision, &op, &data, &at, &actor, &synthetic); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d r%d live=%v | r%d %s %s %s actor=%v synthetic=%d", id, revision, live, hrevision, op, data, at, actor, synthetic))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`7 r1 live=true | r1 insert {"title":"Milk"} 2026-01-02T03:04:05.000Z actor={1 true} synthetic=1`,
		`8 r1 live=true | r1 insert {"title":"Eggs"} 2026-01-03T03:04:05.000Z actor={0 false} synthetic=1`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rows after the upgrade:\n%q\nwant\n%q", got, want)
	}
	// Revisions only go forward, history is never rewritten, and a row's versions are unique.
	for _, bad := range []string{
		`UPDATE app_rows SET revision = 0 WHERE id = 7`,
		`UPDATE app_row_history SET data = '{}' WHERE row_id = 7`,
		`UPDATE app_row_history SET actor = 2 WHERE row_id = 7`,
		`UPDATE app_row_history SET actor = NULL, data = '{}' WHERE row_id = 7`,
		`INSERT INTO app_row_history (app_id, row_id, revision, operation, data) VALUES (5, 7, 1, 'update', '{}')`,
		`INSERT INTO app_row_history (app_id, row_id, revision, operation, data) VALUES (5, 7, 2, 'purge', '{}')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, bad); err == nil {
			t.Fatalf("%s was accepted", bad)
		}
	}
	// An account that goes leaves its history, unattributed (actor ON DELETE SET NULL).
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (3, 'c@example.com', 'C', 'x')`,
		`INSERT INTO app_row_history (app_id, row_id, revision, operation, data, actor) VALUES (5, 7, 2, 'update', '{}', 3)`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM users WHERE id = 3`); err != nil {
		t.Fatalf("deleting a user named in history: %v", err)
	}
	var actor sql.NullInt64
	if err := sqlDB.QueryRowContext(ctx, `SELECT actor FROM app_row_history WHERE row_id = 7 AND revision = 2`).Scan(&actor); err != nil || actor.Valid {
		t.Fatalf("history's actor after the user went = %v, %v", actor, err)
	}
}

// TestRecordSystemActorMigration upgrades a database with records and history from before the
// system writer: nothing in it is a system change, the recreated immutability trigger protects the
// new actor_system column like the others, and the actor is either a user or the system.
func TestRecordSystemActorMigration(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, upTo(t, 45)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x')`,
		`INSERT INTO workspaces (id, name) VALUES (1, 'One')`,
		`INSERT INTO memberships (workspace_id, user_id, role) VALUES (1, 1, 'admin')`,
		`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (10, 1, 'shared', 'Team')`,
		`INSERT INTO apps (id, space_id, name, definition) VALUES (5, 10, 'Tracker', '{}')`,
		`INSERT INTO app_rows (id, app_id, data, created_by, updated_by) VALUES (7, 5, '{"title":"Milk"}', 1, 1)`,
		`INSERT INTO app_row_history (app_id, row_id, revision, operation, data, actor) VALUES (5, 7, 2, 'update', '{}', 1)`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	var updatedSystem, actorSystem sql.NullString
	if err := sqlDB.QueryRowContext(ctx, `SELECT r.updated_system, h.actor_system FROM app_rows r JOIN app_row_history h ON h.row_id = r.id`).Scan(&updatedSystem, &actorSystem); err != nil {
		t.Fatal(err)
	}
	if updatedSystem.Valid || actorSystem.Valid {
		t.Fatalf("rows from before are system changes: %v, %v", updatedSystem, actorSystem)
	}
	for _, q := range []string{
		`UPDATE app_rows SET revision = 3, updated_by = NULL, updated_system = 'integration:4' WHERE id = 7`,
		`INSERT INTO app_row_history (app_id, row_id, revision, operation, data, actor_system, action) VALUES (5, 7, 3, 'update', '{}', 'integration:4', 'payment.succeeded')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// The history of a system change is as immutable as a user's: no column of it is rewritten,
	// the actor_system one included, and it can't be cleared to pose as a deleted account.
	for _, bad := range []string{
		`UPDATE app_row_history SET actor_system = NULL WHERE revision = 3`,
		`UPDATE app_row_history SET actor_system = 'integration:5' WHERE revision = 3`,
		`UPDATE app_row_history SET actor_system = 'integration:4' WHERE revision = 2`,
		`UPDATE app_row_history SET actor = 1 WHERE revision = 3`,
		`UPDATE app_row_history SET action = 'other' WHERE revision = 3`,
		// A change has one actor, a user or the system, and the system's has a known shape.
		`INSERT INTO app_row_history (app_id, row_id, revision, operation, data, actor, actor_system) VALUES (5, 7, 4, 'update', '{}', 1, 'integration:4')`,
		`INSERT INTO app_row_history (app_id, row_id, revision, operation, data, actor_system) VALUES (5, 7, 4, 'update', '{}', 'someone')`,
		`UPDATE app_rows SET updated_system = 'a person' WHERE id = 7`,
		// The id is only digits: a suffix after them is not an integration's.
		`UPDATE app_rows SET updated_system = 'integration:7x' WHERE id = 7`,
		`UPDATE app_rows SET updated_system = 'integration:7:8' WHERE id = 7`,
		`INSERT INTO app_row_history (app_id, row_id, revision, operation, data, actor_system) VALUES (5, 7, 4, 'update', '{}', 'integration:7invalid')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, bad); err == nil {
			t.Fatalf("%s was accepted", bad)
		}
	}
	// An account that goes still leaves its history unattributed, and a system change's history
	// is not touched by it.
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (3, 'c@example.com', 'C', 'x')`,
		`INSERT INTO app_row_history (app_id, row_id, revision, operation, data, actor) VALUES (5, 7, 5, 'update', '{}', 3)`,
		`DELETE FROM users WHERE id = 3`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	rows, err := sqlDB.QueryContext(ctx, `SELECT revision, actor, actor_system FROM app_row_history WHERE revision >= 3 ORDER BY revision`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var revision int
		var actor sql.NullInt64
		var system sql.NullString
		if err := rows.Scan(&revision, &actor, &system); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("r%d actor=%v system=%v", revision, actor.Valid, system.String))
	}
	if want := []string{"r3 actor=false system=integration:4", "r5 actor=false system="}; !slices.Equal(got, want) {
		t.Fatalf("history after the user went = %q, want %q", got, want)
	}
}

// TestAppAttachmentsKeepTheirAppFieldAndUploader checks the attachments table's triggers: an
// attachment never moves to another app, field or uploader, but an account that goes leaves it
// unattributed (owner ON DELETE SET NULL), so deleting a user who uploaded a photo works.
func TestAppAttachmentsKeepTheirAppFieldAndUploader(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef0123456789abcdef"
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x'), (2, 'b@example.com', 'B', 'x')`,
		`INSERT INTO workspaces (id, name) VALUES (1, 'One')`,
		`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (10, 1, 'shared', 'Team')`,
		`INSERT INTO apps (id, space_id, name, definition) VALUES (5, 10, 'Tracker', '{}'), (6, 10, 'Other', '{}')`,
		`INSERT INTO app_attachments (id, app_id, space_id, field, owner, status, size) VALUES ('` + id + `', 5, 10, 'photos', 1, 'uploading', 10)`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{
		`UPDATE app_attachments SET app_id = 6`,
		`UPDATE app_attachments SET field = 'receipt'`,
		`UPDATE app_attachments SET owner = 2`,
	} {
		if _, err := sqlDB.ExecContext(ctx, bad); err == nil {
			t.Fatalf("%s was accepted", bad)
		}
	}
	// Ordinary changes of state still go through.
	if _, err := sqlDB.ExecContext(ctx, `UPDATE app_attachments SET received = 4`); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM users WHERE id = 1`); err != nil {
		t.Fatalf("deleting a user who uploaded an attachment: %v", err)
	}
	var owner sql.NullInt64
	if err := sqlDB.QueryRowContext(ctx, `SELECT owner FROM app_attachments WHERE id = ?`, id).Scan(&owner); err != nil || owner.Valid {
		t.Fatalf("owner after the user went = %v, %v", owner, err)
	}
	// Nobody can be made the uploader of one that has none.
	if _, err := sqlDB.ExecContext(ctx, `UPDATE app_attachments SET owner = 2`); err == nil {
		t.Fatal("an unattributed attachment was given an uploader")
	}
}

// The record of CLI workers marks a database that had users before it as 'unknown' (a container
// worker may have run), and one with none as clean: the purge of a deleted account closes its CLI
// step only for the second.
func TestCLIWorkerHistoryMigrationMarksOlderDatabasesUnknown(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		users bool
		want  []string
	}{{"a database with users", true, []string{"unknown"}}, {"a new database", false, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
			if _, err := migrate(ctx, sqlDB, migrationsBefore(t, "_cli_worker_history.sql")); err != nil {
				t.Fatal(err)
			}
			if tc.users {
				if _, err := sqlDB.ExecContext(ctx, `INSERT INTO users (email, name, password_hash) VALUES ('a@example.com', 'A', 'x')`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Migrate(ctx, sqlDB); err != nil {
				t.Fatal(err)
			}
			rows, err := sqlDB.QueryContext(ctx, `SELECT worker FROM cli_worker_history ORDER BY worker`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var got []string
			for rows.Next() {
				var w string
				if err := rows.Scan(&w); err != nil {
					t.Fatal(err)
				}
				got = append(got, w)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("history = %v, want %v", got, tc.want)
			}
		})
	}
}
