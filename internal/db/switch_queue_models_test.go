package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSwitchQueueMigrationKeepsTheEntriesAndTakesModels(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, migrationsBefore(t, "_switch_queue_models.sql")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x')`,
		`INSERT INTO workspaces (id, name) VALUES (1, 'W')`,
		`INSERT INTO cli_accounts (id, user_id, cli, slot, state) VALUES (7, 1, 'claude', 1, 'connected')`,
		`INSERT INTO cli_shared_accounts (id, workspace_id, cli, label, base_url) VALUES (9, 1, 'claude', 'team', 'https://gateway.example.com')`,
		`INSERT INTO cli_switch_queue (user_id, position, account_id) VALUES (1, 1, 7)`,
		`INSERT INTO cli_switch_queue (user_id, position, shared_account_id) VALUES (1, 2, 9)`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO cli_switch_queue (user_id, position, provider, model_id) VALUES (1, 2, 'p', 'm')`); err == nil {
		t.Fatal("the old table admitted a model")
	}

	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}

	var account int64
	if err := sqlDB.QueryRowContext(ctx, `SELECT account_id FROM cli_switch_queue WHERE user_id = 1 AND position = 1`).Scan(&account); err != nil || account != 7 {
		t.Fatalf("the entry after the migration: %d %v", account, err)
	}
	var shared int64
	if err := sqlDB.QueryRowContext(ctx, `SELECT shared_account_id FROM cli_switch_queue WHERE user_id = 1 AND position = 2`).Scan(&shared); err != nil || shared != 9 {
		t.Fatalf("the shared entry after the migration: %d %v", shared, err)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO cli_switch_queue (user_id, position, shared_account_id) VALUES (1, 9, 9)`); err == nil {
		t.Error("the same shared account twice was admitted")
	}
	type entry struct{ position, account, provider, model any }
	insert := func(e entry) error {
		_, err := sqlDB.ExecContext(ctx, `INSERT INTO cli_switch_queue (user_id, position, account_id, provider, model_id) VALUES (1, ?, ?, ?, ?)`,
			e.position, e.account, e.provider, e.model)
		return err
	}
	if err := insert(entry{3, nil, "u1-api", "big"}); err != nil {
		t.Fatalf("a model: %v", err)
	}
	for name, q := range map[string]entry{
		"a model and an account":   {4, 7, "u1-other", "m"},
		"nothing":                  {4, nil, nil, nil},
		"a provider with no model": {4, nil, "u1-other", nil},
		"a model with no provider": {4, nil, nil, "m"},
		"the same model twice":     {4, nil, "u1-api", "big"},
		"the same position twice":  {3, nil, "u1-api", "other"},
		"the same account twice":   {4, 7, nil, nil},
	} {
		if err := insert(q); err == nil {
			t.Errorf("%s was admitted", name)
		}
	}
	if err := insert(entry{4, nil, "u1-api", "small"}); err != nil {
		t.Fatalf("a second model of one provider: %v", err)
	}
}

func TestSwitchQueueMigrationDeletesAnEntryWithItsSharedAccount(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, migrationsBefore(t, "_switch_queue_models.sql")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x')`,
		`INSERT INTO workspaces (id, name) VALUES (1, 'W')`,
		`INSERT INTO cli_shared_accounts (id, workspace_id, cli, label, base_url) VALUES (9, 1, 'claude', 'team', 'https://gateway.example.com')`,
		`INSERT INTO cli_switch_queue (user_id, position, shared_account_id) VALUES (1, 1, 9)`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM cli_shared_accounts WHERE id = 9`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM cli_switch_queue`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("entries left after the shared account went: %d %v", n, err)
	}
}
