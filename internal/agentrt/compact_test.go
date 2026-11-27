//go:build unix

package agentrt

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func legacyDB(t *testing.T) (path string, marker []byte) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "agent.db")
	marker = []byte("withdrawn-input-" + "4417")
	db := openSQLite(t, path)
	for _, stmt := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA wal_autocheckpoint = 0",
		"PRAGMA secure_delete = OFF",
		"CREATE TABLE entries (id INTEGER PRIMARY KEY, text TEXT)",
	} {
		mustExec(t, db, stmt)
	}
	mustExec(t, db, "INSERT INTO entries (text) VALUES (?), ('kept')", string(marker))
	mustExec(t, db, "DELETE FROM entries WHERE text = ?", string(marker))
	return path, marker
}

func openSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustExec(t *testing.T, db *sql.DB, stmt string, args ...any) {
	t.Helper()
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

func markers(t *testing.T, path string, marker []byte) (inDB, inLog int) {
	t.Helper()
	count := func(p string) int {
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return bytes.Count(b, marker)
	}
	return count(path), count(path + "-wal")
}

func recorded(t *testing.T, path string) bool {
	t.Helper()
	db := openSQLite(t, path)
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'taskto_maintenance'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		return false
	}
	if err := db.QueryRow("SELECT count(*) FROM taskto_maintenance WHERE name = ?", compactedName).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestCompactRecordsOnlyAnEmptiedLog(t *testing.T) {
	ctx := context.Background()
	path, marker := legacyDB(t)
	if inDB, inLog := markers(t, path, marker); inDB+inLog == 0 {
		t.Fatal("the fixture should still hold the deleted row")
	}
	reader := openSQLite(t, path)
	tx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM entries").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := Compact(ctx, path, time.Now()); !errors.Is(err, ErrLogHeld) {
		t.Fatalf("with a reader pinned: %v, want ErrLogHeld", err)
	}
	if recorded(t, path) {
		t.Fatal("recorded the compaction while the log still held what was deleted")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Compact(ctx, path, time.Now()); err != nil {
		t.Fatalf("once the reader is gone: %v", err)
	}
	if !recorded(t, path) {
		t.Fatal("the compaction wasn't recorded")
	}
	if inDB, inLog := markers(t, path, marker); inDB+inLog != 0 {
		t.Fatalf("the deleted row is still in the database (%d) or its log (%d)", inDB, inLog)
	}
}

func TestCompactRefusesARunningRuntimeAndAMissingDatabase(t *testing.T) {
	ctx := context.Background()
	path, _ := legacyDB(t)
	held, err := lockDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Compact(ctx, path, time.Now()); !errors.Is(err, ErrDatabaseInUse) {
		t.Fatalf("with the runtime's lock held: %v, want ErrDatabaseInUse", err)
	}
	_ = held.Close()
	missing := filepath.Join(t.TempDir(), "agent.db")
	if err := Compact(ctx, missing, time.Now()); err == nil {
		t.Fatal("compacted a database that isn't there")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("made %s: %v", missing, err)
	}
}

func TestCompactRecordsWhatTheSidecarReads(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "agent", "src", "storage.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(src, []byte("const MAINTENANCE_TABLE = `"+maintenanceTable+"`")) {
		t.Errorf("storage.ts's MAINTENANCE_TABLE isn't %q", maintenanceTable)
	}
	if !regexp.MustCompile(`const COMPACTED = '` + compactedName + `'`).Match(src) {
		t.Errorf("storage.ts's COMPACTED isn't %q", compactedName)
	}
}
