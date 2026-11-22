// Package db opens the server's SQLite database and keeps its schema up to date.
//
// The driver is modernc.org/sqlite, a translation of SQLite to Go, so the binary
// stays free of cgo and cross-compiles.
//
// Reads and writes: every transaction begins IMMEDIATE (_txlock=immediate), taking the
// write lock at BEGIN, so concurrent writers queue on busy_timeout instead of failing
// mid-transaction. The cost is that transactions don't run in parallel. So:
//   - read with plain Query/QueryRow on the *sql.DB, never inside a transaction: in WAL
//     mode those take no write lock and run alongside a writer;
//   - use a transaction only to write, and keep it short (no network calls inside).
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// FileName is the database's file name inside the data directory.
const FileName = "taskto.db"

// pragmas are applied by the driver to every new connection in the pool.
var pragmas = url.Values{
	// Wait for a lock instead of failing at once with SQLITE_BUSY.
	"_busy_timeout": {"5000"},
	"_foreign_keys": {"1"},
	// Readers don't block the writer, and the writer doesn't block readers.
	"_journal_mode": {"WAL"},
	// Durable across a process crash; a power cut may lose the last transactions
	// but never corrupts the database. The usual choice with WAL.
	"_synchronous": {"NORMAL"},
	// Write transactions take the write lock when they begin, so two of them wait
	// for each other (busy_timeout) instead of one failing when it first writes.
	"_txlock": {"immediate"},
}

// Open opens the database at path, creating the file if needed, and checks that it
// can be reached. It doesn't apply migrations; see Migrate.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	dsn, err := dsn(path)
	if err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	return sqlDB, nil
}

// dsn turns a file path into a SQLite URI carrying the pragmas. Using a URI, rather
// than a plain name, lets the path contain '?' or '#'.
func dsn(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("database path %s: %w", path, err)
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows: C:/x becomes file:///C:/x
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String() + "?" + pragmas.Encode(), nil
}
