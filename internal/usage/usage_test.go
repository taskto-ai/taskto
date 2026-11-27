package usage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"taskto.ai/taskto/internal/db"
)

func open(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(ctx, d); err != nil {
		t.Fatal(err)
	}
	return d
}

func user(t *testing.T, d *sql.DB, email string) int64 {
	t.Helper()
	r, err := d.Exec(`INSERT INTO users (email, name, password_hash) VALUES (?, 'A', '')`, email)
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRecordAggregatesByUTCDateAndDailyScopesTheOwner(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	ws, err := d.Exec(`INSERT INTO workspaces (name) VALUES ('Home')`)
	if err != nil {
		t.Fatal(err)
	}
	workspace, _ := ws.LastInsertId()
	one, two := user(t, d, "one@example.test"), user(t, d, "two@example.test")
	first := time.Date(2026, 11, 27, 23, 30, 0, 0, time.FixedZone("west", -5*60*60))
	for _, entry := range []Entry{
		{User: one, Workspace: workspace, Source: "cli", Engine: "codex", Input: 4, Output: 2, At: first},
		{User: one, Workspace: workspace, Source: "cli", Engine: "codex", Input: 6, Output: 3, At: first.Add(time.Minute)},
		{User: one, Workspace: workspace, Source: "agent", Engine: "provider-a", Input: 7, Output: 5, At: first.Add(2 * time.Hour)},
		{User: two, Workspace: workspace, Source: "cli", Engine: "codex", Input: 900, Output: 800, At: first},
	} {
		if err := Record(ctx, d, entry); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Daily(ctx, d, one, workspace, time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (Day{Day: "2026-11-28", Source: "agent", Engine: "provider-a", InputTokens: 7, OutputTokens: 5, Turns: 1}) || got[1] != (Day{Day: "2026-11-28", Source: "cli", Engine: "codex", InputTokens: 10, OutputTokens: 5, Turns: 2}) {
		t.Fatalf("Daily = %+v", got)
	}
	got, err = Daily(ctx, d, two, workspace, first, first.Add(24*time.Hour))
	if err != nil || len(got) != 1 || got[0].InputTokens != 900 {
		t.Fatalf("other user's Daily = %+v, %v", got, err)
	}
}

func TestRecordPrunesOlderThan400DaysAndAccountDeletionCascades(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	res, err := d.Exec(`INSERT INTO workspaces (name) VALUES ('Home')`)
	if err != nil {
		t.Fatal(err)
	}
	workspace, _ := res.LastInsertId()
	one, two := user(t, d, "one@example.test"), user(t, d, "two@example.test")
	old := time.Now().UTC().AddDate(0, 0, -401)
	if err := Record(ctx, d, Entry{User: one, Workspace: workspace, Source: "cli", Engine: "claude", Input: 1, At: old}); err != nil {
		t.Fatal(err)
	}
	if err := Record(ctx, d, Entry{User: two, Workspace: workspace, Source: "cli", Engine: "claude", Input: 1, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM agent_usage_daily WHERE user_id = ?`, one).Scan(&count); err != nil || count != 0 {
		t.Fatalf("old rows = %d, %v", count, err)
	}
	if _, err := d.Exec(`DELETE FROM users WHERE id = ?`, two); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM agent_usage_daily WHERE user_id = ?`, two).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rows after account deletion = %d, %v", count, err)
	}
}
