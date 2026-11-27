package db

import (
	"context"
	"path/filepath"
	"testing"
)

// The payment links' migration rebuilds app_effects so its kind admits create_payment_link. What a
// database had keeps, and so does the table's sequence: an entry's number is never given again after
// the entry was cleaned up, since cards, documents and payment references remember the entry by it.
func TestPaymentLinksMigrationKeepsTheEntriesAndTheirSequence(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, migrationsBefore(t, "_payment_links.sql")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x')`,
		`INSERT INTO workspaces (id, name) VALUES (1, 'W')`,
		`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (1, 1, 'shared', 'Team')`,
		`INSERT INTO apps (id, space_id, name, definition) VALUES (1, 1, 'Invoices', '{}')`,
		`INSERT INTO app_rows (id, app_id, data, created_by) VALUES (1, 1, '{}', 1)`,
		`INSERT INTO app_operations (id, app_id, user_id, operation, action, request_hash, record_id) VALUES (1, 1, 1, 'op-0000001', 'send', 'h', 1)`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Three entries, then the two newest are cleaned up: the sequence is at 3, the table's highest
	// number 1.
	for i := 0; i < 3; i++ {
		if _, err := sqlDB.ExecContext(ctx, `INSERT INTO app_effects (app_id, operation_id, position, kind, record_id, record_revision, user_id, action_kind, payload, state, next_attempt_at)
			VALUES (1, 1, ?, 'create_card', 1, 1, 1, 'update', '{"title":"t"}', 'done', '2026-01-01T00:00:00.000Z')`, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM app_effects WHERE id > 1`); err != nil {
		t.Fatal(err)
	}
	// The old table refuses the new kind.
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO app_effects (app_id, operation_id, position, kind, record_id, record_revision, user_id, action_kind, payload, next_attempt_at)
		VALUES (1, 1, 9, 'create_payment_link', 1, 1, 1, 'update', '{}', '2026-01-01T00:00:00.000Z')`); err == nil {
		t.Fatal("the old table admitted create_payment_link")
	}

	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}

	var kind, state string
	if err := sqlDB.QueryRowContext(ctx, `SELECT kind, state FROM app_effects WHERE id = 1`).Scan(&kind, &state); err != nil || kind != "create_card" || state != "done" {
		t.Fatalf("the entry after the migration: %s %s %v", kind, state, err)
	}
	var id int64
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO app_effects (app_id, operation_id, position, kind, record_id, record_revision, user_id, action_kind, payload, next_attempt_at)
		VALUES (1, 1, 9, 'create_payment_link', 1, 1, 1, 'update', '{}', '2026-01-01T00:00:00.000Z') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("the new kind: %v", err)
	}
	if id != 4 {
		t.Fatalf("the next entry is %d: a number given before (up to 3) was given again", id)
	}
	// The other kinds' rules stand: a kind that isn't one is refused, as are an operation's position twice.
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO app_effects (app_id, operation_id, position, kind, record_id, record_revision, user_id, action_kind, payload, next_attempt_at)
		VALUES (1, 1, 10, 'send_email', 1, 1, 1, 'update', '{}', '2026-01-01T00:00:00.000Z')`); err == nil {
		t.Fatal("an unknown kind was admitted")
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO app_effects (app_id, operation_id, position, kind, record_id, record_revision, user_id, action_kind, payload, next_attempt_at)
		VALUES (1, 1, 9, 'create_card', 1, 1, 1, 'update', '{}', '2026-01-01T00:00:00.000Z')`); err == nil {
		t.Fatal("an operation's position was used twice")
	}
	// The indexes are back.
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'app_effects' AND name IN ('app_effects_due', 'app_effects_by_app', 'app_effects_by_record')`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("indexes = %d, %v", n, err)
	}
}

// A database whose table never held an entry starts at 1, as a fresh one does.
func TestPaymentLinksMigrationOnAnEmptyTable(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_sequence WHERE name = 'app_effects_new'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a leftover sequence of the temporary table: %d, %v", n, err)
	}
}

// A reference belongs to one outbox entry, and a token is the 128 random bits the server issues.
func TestIntegrationRefsAreOnePerEntryAndSurviveItsCleanup(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x')`,
		`INSERT INTO workspaces (id, name) VALUES (1, 'W')`,
		`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (1, 1, 'shared', 'Team')`,
		`INSERT INTO apps (id, space_id, name, definition) VALUES (1, 1, 'Invoices', '{}')`,
		`INSERT INTO app_rows (id, app_id, data, created_by) VALUES (1, 1, '{}', 1)`,
		`INSERT INTO app_operations (id, app_id, user_id, operation, action, request_hash, record_id) VALUES (1, 1, 1, 'op-0000001', 'send', 'h', 1)`,
		`INSERT INTO app_effects (id, app_id, operation_id, position, kind, record_id, record_revision, user_id, action_kind, payload, next_attempt_at)
			VALUES (7, 1, 1, 0, 'create_payment_link', 1, 1, 1, 'update', '{}', '2026-01-01T00:00:00.000Z')`,
		`INSERT INTO integrations (id, workspace_id, public_id, provider, name, display_name, mode, max_amount_minor, currencies)
			VALUES (1, 1, lower(hex(randomblob(16))), 'stripe', 'main', 'main', 'test', 1000, '["EUR"]')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	insert := func(token string, effect any, amount int64) error {
		_, err := sqlDB.ExecContext(ctx, `INSERT INTO integration_refs (token, integration_id, app_id, record_id, slot, effect_id, amount_minor, currency)
			VALUES (?, 1, 1, 1, 'payments', ?, ?, 'EUR')`, token, effect, amount)
		return err
	}
	const token = "test-token-not-real-000000000001"
	if err := insert(token, 7, 1000); err != nil {
		t.Fatal(err)
	}
	if err := insert("test-token-not-real-000000000002", 7, 1000); err == nil {
		t.Fatal("an entry has two refs")
	}
	if err := insert(token, nil, 1000); err == nil {
		t.Fatal("a token was used twice")
	}
	if err := insert("short", nil, 1000); err == nil {
		t.Fatal("a token that isn't 128 bits")
	}
	if err := insert("test-token-not-real-000000000003", nil, 0); err == nil {
		t.Fatal("an amount of zero")
	}
	// A ref's keys: one row per key, each with a token of its own; a call is never counted as clean more
	// often than it was sent, and an outcome is one of the four.
	key := func(seq int, token string, sends, clean int, outcome any) error {
		_, err := sqlDB.ExecContext(ctx, `INSERT INTO integration_ref_attempts (ref_id, seq, token, sends, clean_sends, outcome)
			SELECT id, ?, ?, ?, ?, ? FROM integration_refs WHERE effect_id = 7`, seq, token, sends, clean, outcome)
		return err
	}
	if err := key(0, token, 1, 0, nil); err != nil {
		t.Fatal(err)
	}
	for what, err := range map[string]error{
		"a key twice":               key(0, "test-token-not-real-000000000004", 1, 0, nil),
		"a token of two keys":       key(1, token, 1, 0, nil),
		"more clean calls than any": key(1, "test-token-not-real-000000000005", 1, 2, nil),
		"an outcome not said":       key(1, "test-token-not-real-000000000006", 1, 0, "lost"),
	} {
		if err == nil {
			t.Fatalf("%s was stored", what)
		}
	}
	// An object is checked only if there is one: a key with no object can't say its object was checked.
	if _, err := sqlDB.ExecContext(ctx, `UPDATE integration_ref_attempts SET object_verified = 1 WHERE seq = 0`); err == nil {
		t.Fatal("a key with no object was marked as having a checked one")
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE integration_ref_attempts SET object_id = 'plink_seen', object_verified = 0 WHERE seq = 0`); err != nil {
		t.Fatalf("an object seen and not checked: %v", err)
	}
	// The entry is cleaned up: the ref stays, forgetting only the entry.
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM app_effects WHERE id = 7`); err != nil {
		t.Fatal(err)
	}
	var effect *int64
	if err := sqlDB.QueryRowContext(ctx, `SELECT effect_id FROM integration_refs WHERE token = ?`, token).Scan(&effect); err != nil || effect != nil {
		t.Fatalf("the ref after its entry was cleaned up: %v %v", effect, err)
	}
	// An integration that made links can't be deleted; an app's deletion takes its refs.
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM integrations WHERE id = 1`); err == nil {
		t.Fatal("an integration with refs was deleted")
	}
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM apps WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM integration_refs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refs left after the app: %d %v", n, err)
	}
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM integration_ref_attempts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("keys left after their ref: %d %v", n, err)
	}
}

// A reference made before it kept its money's fields takes them from its entry, while the entry is
// kept, and one whose entry was cleaned up keeps ” (which locks every field any release of its app
// read money from).
func TestIntegrationRefsTakeTheirMoneyFieldsFromTheirEntry(t *testing.T) {
	ctx := context.Background()
	sqlDB := open(t, filepath.Join(t.TempDir(), FileName))
	if _, err := migrate(ctx, sqlDB, migrationsBefore(t, "_integration_ref_field.sql")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, email, name, password_hash) VALUES (1, 'a@example.com', 'A', 'x')`,
		`INSERT INTO workspaces (id, name) VALUES (1, 'W')`,
		`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (1, 1, 'shared', 'Team')`,
		`INSERT INTO apps (id, space_id, name, definition) VALUES (1, 1, 'Invoices', '{}')`,
		`INSERT INTO app_rows (id, app_id, data, created_by) VALUES (1, 1, '{}', 1)`,
		`INSERT INTO app_operations (id, app_id, user_id, operation, action, request_hash, record_id) VALUES (1, 1, 1, 'op-0000001', 'send', 'h', 1)`,
		`INSERT INTO app_effects (id, app_id, operation_id, position, kind, record_id, record_revision, user_id, action_kind, payload, next_attempt_at)
			VALUES (7, 1, 1, 0, 'create_payment_link', 1, 1, 1, 'update', '{"url_field":"pay_url","id_field":"payment_id","amount_field":"total","currency_field":"currency"}', '2026-01-01T00:00:00.000Z')`,
		`INSERT INTO integrations (id, workspace_id, public_id, provider, name, display_name, mode, max_amount_minor, currencies)
			VALUES (1, 1, lower(hex(randomblob(16))), 'stripe', 'main', 'main', 'test', 1000, '["EUR"]')`,
		`INSERT INTO integration_refs (token, integration_id, app_id, record_id, slot, effect_id, amount_minor, currency)
			VALUES ('test-token-not-real-000000000001', 1, 1, 1, 'payments', 7, 1000, 'EUR')`,
		`INSERT INTO integration_refs (token, integration_id, app_id, record_id, slot, effect_id, amount_minor, currency)
			VALUES ('test-token-not-real-000000000002', 1, 1, 1, 'payments', NULL, 1000, 'EUR')`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	for token, want := range map[string]string{"test-token-not-real-000000000001": "total:currency", "test-token-not-real-000000000002": ":"} {
		var fields string
		if err := sqlDB.QueryRowContext(ctx, `SELECT amount_field || ':' || currency_field FROM integration_refs WHERE token = ?`, token).Scan(&fields); err != nil || fields != want {
			t.Fatalf("the fields of %s = %q, %v; want %q", token, fields, err, want)
		}
	}
}
