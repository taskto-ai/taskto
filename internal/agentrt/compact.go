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

	_ "modernc.org/sqlite"
)

var ErrLogHeld = errors.New("a reader holds the agent runtime's write-ahead log, so it couldn't be emptied: stop backups and anything else that reads agent.db, and run it again")

const (
	maintenanceTable = `CREATE TABLE IF NOT EXISTS taskto_maintenance (
  name    TEXT PRIMARY KEY,
  done_at INTEGER NOT NULL
) STRICT`
	compactedName = "secure_delete_vacuum"
)

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
	if busy != 0 || logFrames > 0 {
		return ErrLogHeld
	}
	if wal, err := os.Stat(path + "-wal"); err == nil && wal.Size() != 0 {
		return ErrLogHeld
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("compact %s: %w", path, err)
	}
	if _, err := conn.ExecContext(ctx, maintenanceTable); err != nil {
		return fmt.Errorf("compact %s: record it: %w", path, err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT OR REPLACE INTO taskto_maintenance (name, done_at) VALUES (?, ?)",
		compactedName, now.UnixMilli()); err != nil {
		return fmt.Errorf("compact %s: record it: %w", path, err)
	}
	return nil
}
