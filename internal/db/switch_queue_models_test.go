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
		`INSERT INTO cli_switch_queue (user_id, position, account_id) VALUES (1, 1, 7)`,
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
	type entry struct{ position, account, provider, model any }
	insert := func(e entry) error {
		_, err := sqlDB.ExecContext(ctx, `INSERT INTO cli_switch_queue (user_id, position, account_id, provider, model_id) VALUES (1, ?, ?, ?, ?)`,
			e.position, e.account, e.provider, e.model)
		return err
	}
	if err := insert(entry{2, nil, "u1-api", "big"}); err != nil {
		t.Fatalf("a model: %v", err)
	}
	for name, q := range map[string]entry{
		"a model and an account":   {3, 7, "u1-other", "m"},
		"nothing":                  {3, nil, nil, nil},
		"a provider with no model": {3, nil, "u1-other", nil},
		"a model with no provider": {3, nil, nil, "m"},
		"the same model twice":     {3, nil, "u1-api", "big"},
		"the same position twice":  {2, nil, "u1-api", "other"},
		"the same account twice":   {3, 7, nil, nil},
	} {
		if err := insert(q); err == nil {
			t.Errorf("%s was admitted", name)
		}
	}
	if err := insert(entry{3, nil, "u1-api", "small"}); err != nil {
		t.Fatalf("a second model of one provider: %v", err)
	}
}
