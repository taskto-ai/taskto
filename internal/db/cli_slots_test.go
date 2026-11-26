package db

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// migrationsBefore returns the embedded migrations that come before the one whose name ends
// in suffix.
func migrationsBefore(t *testing.T, suffix string) fstest.MapFS {
	t.Helper()
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		t.Fatal(err)
	}
	out := fstest.MapFS{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			return out
		}
		data, err := fs.ReadFile(sub, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = &fstest.MapFile{Data: data}
	}
	t.Fatalf("no migration named *%s", suffix)
	return nil
}

// Several accounts per CLI rebuild cli_accounts and cli_sessions: what a database had keeps,
// as each user's first account, with its sessions.
func TestCLIAccountSlotsKeepTheAccounts(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, migrationsBefore(t, "_cli_account_slots.sql")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x')`,
		`INSERT INTO workspaces (id, name) VALUES (1, 'W')`,
		`INSERT INTO conversations (id, workspace_id, owner_id, agent_id) VALUES (5, 1, 1, 9)`,
		`INSERT INTO cli_accounts (id, user_id, cli, state, auth_method, limits) VALUES (3, 1, 'codex', 'reconnect', 'chatgpt', '[]')`,
		`INSERT INTO cli_sessions (conversation_id, account_id, session_id) VALUES (5, 3, 'thread-1')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	var slot int
	var state, method, session string
	if err := sqlDB.QueryRowContext(ctx, `SELECT a.slot, a.state, a.auth_method, s.session_id FROM cli_accounts a
		JOIN cli_sessions s ON s.account_id = a.id WHERE a.id = 3 AND s.conversation_id = 5`).Scan(&slot, &state, &method, &session); err != nil {
		t.Fatal(err)
	}
	if slot != 1 || state != "reconnect" || method != "chatgpt" || session != "thread-1" {
		t.Fatalf("after the migration: slot %d, state %s, method %s, session %s", slot, state, method, session)
	}
	// A second account of the same CLI now fits; a second first one doesn't.
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO cli_accounts (user_id, cli, slot, state) VALUES (1, 'codex', 2, 'connected')`); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO cli_accounts (user_id, cli, slot, state) VALUES (1, 'codex', 1, 'connected')`); err == nil {
		t.Fatal("two first accounts for one CLI")
	}
	// The sessions still go with their account.
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM cli_accounts WHERE id = 3`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM cli_sessions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("sessions left = %d %v", n, err)
	}
}

// A purge's CLI home step recorded before the browser's step existed gets a browser step, which
// the purge does first; the rows and their numbers stay.
func TestPurgeCleanupsGainTheBrowserStep(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, migrationsBefore(t, "_purge_cleanup_browser.sql")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO purge_cleanups (user_id, kind, target, attempts, last_error) VALUES (7, 'cli_home', '7', 2, 'cli_worker_failed')`,
		`INSERT INTO purge_cleanups (user_id, kind, target) VALUES (7, 'providers', 'feed:7')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Up to this migration only: the CLI step's target changes in a later one (0059, its own test).
	if _, err := migrate(ctx, sqlDB, upTo(t, 58)); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	rows, err := sqlDB.QueryContext(ctx, `SELECT kind || ':' || target || ':' || attempts FROM purge_cleanups ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, s)
	}
	want := "cli_home:7:2 providers:feed:7:0 browser:7:0"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("steps = %q, want %q", got, want)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO purge_cleanups (user_id, kind, target) VALUES (8, 'browser', '8')`); err != nil {
		t.Fatalf("a browser step can't be recorded: %v", err)
	}
}
