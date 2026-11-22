package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
)

// migrations holds the schema changes, applied in order of their number. A migration
// is never edited once it has been merged: change the schema with a new file.
//
//go:embed migrations/*.sql
var migrations embed.FS

// migrationName is NNNN_description.sql; NNNN is the version.
var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

type migration struct {
	version  int
	name     string
	sql      string
	checksum string // sha256 of the file, to catch a migration edited after it ran
}

// record is what schema_migrations holds about an applied migration.
type record struct {
	name     string
	checksum string
}

// reworded lists the migrations whose comments were reworded after they had run, with
// nothing else in them changed. A database that recorded the file as it was (before)
// still starts. TestRewordedMigrationsChangedOnlyComments checks that sqlOnly, the
// checksum of the file without its comment lines, is still what the database ran.
// Never add to this to change a migration's SQL: write a new migration.
var reworded = map[string]struct{ before, sqlOnly string }{
	"0014_cli_account_slots.sql": {
		before:  "20b68b60a148c16a28beb3e590798493d79ddb54bb4f572ed0e211c025ef9f38",
		sqlOnly: "d0690803e2333fb20a29ab42d3012150e720b8a6aeb5dacc8b5ad64544268d45",
	},
	"0015_cli_shared_accounts.sql": {
		before:  "b457952814aaacaf625fe69b1c83187c31a7148c5144678397edfd9129abcd13",
		sqlOnly: "e74a076b92688beeca3ed7469d4e5f5f90f24e91974aa745f755050f5da44daf",
	},
	"0016_cli_switching.sql": {
		before:  "2c097e636747693d7be637411e85e1898f2034de9ac3ae933950c19a8e70749e",
		sqlOnly: "3ba8ba7c74c15ac8aeb6b7660e831dcaa1899182bdfb5ef9abc5a078b5228038",
	},
	"0018_dictation_local_default.sql": {
		before:  "b80b66ea80aaf96cb60464fd53ff34b5a45846741a35063540980752eb777487",
		sqlOnly: "9013818854cdbfff9397a41d2dba072d610878d3335d2cc8d5f6b4e9d07ceaa3",
	},
}

// sameMigration says whether rec, as a database recorded it, is m: the same file, or
// one whose comments were reworded since (reworded).
func sameMigration(rec record, m migration) bool {
	if rec.name != m.name {
		return false
	}
	r, ok := reworded[m.name]
	return rec.checksum == m.checksum || ok && rec.checksum == r.before
}

// Migrate applies the migrations the database doesn't have yet, each in its own
// transaction, and returns the names of those it applied. It stops at the first one
// that fails, leaving the database as that migration found it.
func Migrate(ctx context.Context, sqlDB *sql.DB) ([]string, error) {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	return migrate(ctx, sqlDB, sub)
}

func migrate(ctx context.Context, sqlDB *sql.DB, fsys fs.FS) ([]string, error) {
	all, err := load(fsys)
	if err != nil {
		return nil, err
	}

	if _, err := sqlDB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		checksum   TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
	)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	done, err := appliedVersions(ctx, sqlDB)
	if err != nil {
		return nil, err
	}
	for version, rec := range done {
		if !slices.ContainsFunc(all, func(m migration) bool { return m.version == version }) {
			return nil, fmt.Errorf("the database has migration %s, which this binary doesn't know: it was written by a newer version of TaskTo.AI", rec.name)
		}
	}
	// A migration is never edited or renamed once it has run anywhere: that database would
	// silently differ from a fresh one. Say so instead of starting.
	for _, m := range all {
		if rec, ok := done[m.version]; ok && !sameMigration(rec, m) {
			return nil, fmt.Errorf("migration %04d changed after it was applied (recorded as %s): add a new migration instead of editing one", m.version, rec.name)
		}
	}

	var applied []string
	for _, m := range all {
		if _, ok := done[m.version]; ok {
			continue
		}
		ran, err := apply(ctx, sqlDB, m)
		if err != nil {
			return applied, fmt.Errorf("migration %s: %w", m.name, err)
		}
		if ran {
			applied = append(applied, m.name)
		}
	}
	return applied, nil
}

// load reads and orders the migration files.
func load(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var all []migration
	for _, e := range entries {
		match := migrationName.FindStringSubmatch(e.Name())
		if e.IsDir() || match == nil {
			return nil, fmt.Errorf("migrations: %q isn't named NNNN_description.sql", e.Name())
		}
		version, _ := strconv.Atoi(match[1])
		raw, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		all = append(all, migration{version: version, name: e.Name(), sql: string(raw), checksum: hex.EncodeToString(sum[:])})
	}
	slices.SortFunc(all, func(a, b migration) int { return a.version - b.version })
	for i := 1; i < len(all); i++ {
		if all[i].version == all[i-1].version {
			return nil, fmt.Errorf("migrations %s and %s have the same number", all[i-1].name, all[i].name)
		}
	}
	return all, nil
}

func appliedVersions(ctx context.Context, sqlDB *sql.DB) (map[int]record, error) {
	rows, err := sqlDB.QueryContext(ctx, `SELECT version, name, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	done := map[int]record{}
	for rows.Next() {
		var version int
		var rec record
		if err := rows.Scan(&version, &rec.name, &rec.checksum); err != nil {
			return nil, err
		}
		done[version] = rec
	}
	return done, rows.Err()
}

// apply runs one migration and records it, in a single transaction. It reports false
// if another process applied the migration first.
func apply(ctx context.Context, sqlDB *sql.DB, m migration) (ran bool, err error) {
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(); !errors.Is(rbErr, sql.ErrTxDone) {
				err = errors.Join(err, rbErr)
			}
		}
	}()

	// The transaction holds the write lock from its start (_txlock=immediate), so this
	// check and the migration can't interleave with another process doing the same.
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = ?)`, m.version).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, tx.Rollback()
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES (?, ?, ?)`, m.version, m.name, m.checksum); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
