package agentrt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// ErrLogHeld is Compact's answer when the agent runtime's write-ahead log couldn't be emptied
// after the rewrite: a reader (a backup's snapshot, a sqlite3 shell) still holds it, and what was
// deleted can still be in it. Nothing is recorded; run it again once the reader is gone.
var ErrLogHeld = errors.New("a reader holds the agent runtime's write-ahead log, so it couldn't be emptied: stop backups and anything else that reads agent.db, and run it again")

// The sidecar's record that its database was compacted, as agent/src/storage.ts writes and reads
// it (a test checks the two agree): with the row, the sidecar's start doesn't compact again.
const (
	maintenanceTable = `CREATE TABLE IF NOT EXISTS taskto_maintenance (
  name    TEXT PRIMARY KEY,
  done_at INTEGER NOT NULL
) STRICT`
	compactedName = "secure_delete_vacuum"
)

// Compact compacts the agent runtime's database at path by hand, for one the sidecar leaves
// alone at its start (larger than it compacts then: docs/install.md#privacy-and-account-deletion).
// It takes the runtime's own lock (DB + ".lock"), so it refuses while a sidecar has the database,
// and it never creates one. It rewrites every page with secure_delete on (VACUUM), then empties
// the write-ahead log (a TRUNCATE checkpoint), and only when SQLite says that checkpoint wasn't
// held up (busy = 0) and the log is empty, file and all, it records the compaction, as a separate
// step: a reader that held the log keeps what was deleted in it, and then nothing is recorded
// (ErrLogHeld). It needs room on the disk for a copy of the database.
func Compact(ctx context.Context, path string, now time.Time) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("agent runtime database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("agent runtime database %s: not a regular file", path)
	}
	lock, err := lockDB(path)
	if err != nil {
		return err
	}
	defer lock.Close()
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// mode=rw: never makes a database that isn't there. No busy timeout: a writer or a checkpoint
	// that holds a lock is an answer, not something to wait for.
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "mode=rw&_pragma=busy_timeout(0)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer conn.Close()
	for _, stmt := range []string{"PRAGMA secure_delete = ON", "VACUUM"} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("compact %s: %s: %w", path, stmt, err)
		}
	}
	var busy, logFrames, checkpointed int64
	if err := conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("compact %s: empty the write-ahead log: %w", path, err)
	}
	// busy = 1: a reader or another checkpoint held it up; log > 0: frames are left in it. log is
	// -1 only when the database isn't in WAL mode, and then there is no log to hold anything.
	if busy != 0 || logFrames > 0 {
		return ErrLogHeld
	}
	if wal, err := os.Stat(path + "-wal"); err == nil && wal.Size() != 0 {
		return ErrLogHeld
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("compact %s: %w", path, err)
	}
	// The rewrite is in the file and the log is empty: only now is it recorded.
	if _, err := conn.ExecContext(ctx, maintenanceTable); err != nil {
		return fmt.Errorf("compact %s: record it: %w", path, err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT OR REPLACE INTO taskto_maintenance (name, done_at) VALUES (?, ?)",
		compactedName, now.UnixMilli()); err != nil {
		return fmt.Errorf("compact %s: record it: %w", path, err)
	}
	return nil
}
