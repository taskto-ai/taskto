package audit_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/audit/audittest"
	"taskto.ai/taskto/internal/db"
)

func open(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	return sqlDB
}

func exec(t *testing.T, d *sql.DB, query string, args ...any) int64 {
	t.Helper()
	res, err := d.Exec(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestRecordStoresTheEvent(t *testing.T) {
	d := open(t)
	ws := exec(t, d, `INSERT INTO workspaces (name) VALUES ('Home')`)
	user := exec(t, d, `INSERT INTO users (email, name, password_hash) VALUES ('a@example.com', 'A', '')`)

	ctx := audit.WithClient(audit.WithActor(context.Background(), user), "203.0.113.9")
	err := audit.Record(ctx, d, audit.Event{Workspace: ws, Action: audit.SpaceCreated, TargetType: audit.TargetSpace, Target: 7,
		Details: audit.Details{"name": "Team", "archived": false, "n": 3, "fields": []string{"a", "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := audit.List(context.Background(), d, audit.Filter{Workspace: ws})
	if err != nil || len(rows) != 1 {
		t.Fatalf("List = %+v, %v", rows, err)
	}
	r := rows[0]
	if *r.WorkspaceID != ws || *r.ActorID != user || r.Action != "space_created" || r.TargetType != "space" || *r.TargetID != 7 ||
		*r.ClientAddr != "203.0.113.9" || r.Details["name"] != "Team" || r.Details["archived"] != false {
		t.Fatalf("row = %+v", r)
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", r.At); err != nil {
		t.Fatalf("time %q: %v", r.At, err)
	}
}

func TestAnEventNeedsNoActorWorkspaceTargetOrAddress(t *testing.T) {
	d := open(t)
	if err := audit.Record(context.Background(), d, audit.Event{Action: audit.LoginFailed}); err != nil {
		t.Fatal(err)
	}
	rows, _ := audit.List(context.Background(), d, audit.Filter{})
	r := rows[0]
	if r.WorkspaceID != nil || r.ActorID != nil || r.TargetID != nil || r.ClientAddr != nil || len(r.Details) != 0 {
		t.Fatalf("row = %+v, want nulls and no details", r)
	}
}

func TestAnExplicitActorBeatsTheRequests(t *testing.T) {
	d := open(t)
	a := exec(t, d, `INSERT INTO users (email, name, password_hash) VALUES ('a@example.com', 'A', '')`)
	b := exec(t, d, `INSERT INTO users (email, name, password_hash) VALUES ('b@example.com', 'B', '')`)
	ctx := audit.WithActor(context.Background(), a)
	if err := audit.Record(ctx, d, audit.Event{Actor: b, Action: audit.InviteAccepted}); err != nil {
		t.Fatal(err)
	}
	rows, _ := audit.List(context.Background(), d, audit.Filter{})
	if *rows[0].ActorID != b {
		t.Fatalf("actor = %d, want %d", *rows[0].ActorID, b)
	}
}

func TestRecordRefusesWhatCouldHoldASecret(t *testing.T) {
	d := open(t)
	bad := []struct {
		name    string
		action  string
		details audit.Details
	}{
		{"an action that isn't snake_case", "Login Succeeded", nil},
		{"an empty action", "", nil},
		{"a password", audit.PasswordChanged, audit.Details{"password": "hunter2-hunter2"}},
		{"a new password", audit.PasswordChanged, audit.Details{"new_password": "x"}},
		{"a token", audit.AccessTokenCreated, audit.Details{"token": "ttp_abc"}},
		{"an api key", audit.ProviderAccountCreated, audit.Details{"api_key": "sk-123"}},
		{"a key by another name", audit.ProviderAccountCreated, audit.Details{"Secret": "x"}},
		{"a reset link", audit.PasswordResetCreated, audit.Details{"reset_link": "https://x/y"}},
		{"a session", audit.Logout, audit.Details{"session_id": "abc"}},
		{"a hash", audit.InviteCreated, audit.Details{"token_hash": "abc"}},
		{"a list under a secret's name", audit.InviteCreated, audit.Details{"keys": []string{"a"}}},
		{"a nested value", audit.InviteCreated, audit.Details{"user": map[string]string{"name": "x"}}},
		{"a byte slice", audit.InviteCreated, audit.Details{"blob": []byte("x")}},
		{"too many details", audit.InviteCreated, tooMany()},
	}
	for _, c := range bad {
		if err := audit.Record(context.Background(), d, audit.Event{Action: c.action, Details: c.details}); err == nil {
			t.Errorf("%s: recorded", c.name)
		}
	}
	if rows, _ := audit.List(context.Background(), d, audit.Filter{}); len(rows) != 0 {
		t.Fatalf("%d events were stored: %+v", len(rows), rows)
	}

	// A boolean may carry a secret's name: it says the key changed, not what it is.
	if err := audit.Record(context.Background(), d, audit.Event{Action: audit.ProviderAccountUpdated,
		Details: audit.Details{"api_key_changed": true}}); err != nil {
		t.Fatal(err)
	}
}

func tooMany() audit.Details {
	d := audit.Details{}
	for _, k := range strings.Split("abcdefghijklmnopq", "") {
		d[k] = "x"
	}
	return d
}

func TestLongDetailsAreClipped(t *testing.T) {
	d := open(t)
	long := strings.Repeat("é", 500)
	if err := audit.Record(context.Background(), d, audit.Event{Action: audit.SpaceCreated, Details: audit.Details{"name": long}}); err != nil {
		t.Fatal(err)
	}
	rows, _ := audit.List(context.Background(), d, audit.Filter{})
	if got := rows[0].Details["name"].(string); len([]rune(got)) != 200 {
		t.Fatalf("name has %d characters, want 200", len([]rune(got)))
	}
}

func TestAFailedChangeTakesItsEventWithIt(t *testing.T) {
	d := open(t)
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.Record(context.Background(), tx, audit.Event{Action: audit.MemberRemoved}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if rows, _ := audit.List(context.Background(), d, audit.Filter{}); len(rows) != 0 {
		t.Fatalf("a rolled-back event was stored: %+v", rows)
	}
}

func TestLogSwallowsItsFailure(t *testing.T) {
	d := open(t)
	// Not a valid action: Record fails, Log doesn't.
	audit.Log(context.Background(), d, audit.Event{Action: "NOT VALID"})
	if rows, _ := audit.List(context.Background(), d, audit.Filter{}); len(rows) != 0 {
		t.Fatalf("events = %+v", rows)
	}
	audit.Log(context.Background(), d, audit.Event{Action: audit.LoginFailed})
	if rows, _ := audit.List(context.Background(), d, audit.Filter{}); len(rows) != 1 {
		t.Fatalf("events = %+v", rows)
	}
}

func TestAWorkspacesEventsGoWithIt(t *testing.T) {
	d := open(t)
	gone := exec(t, d, `INSERT INTO workspaces (name) VALUES ('Gone')`)
	kept := exec(t, d, `INSERT INTO workspaces (name) VALUES ('Kept')`)
	user := exec(t, d, `INSERT INTO users (email, name, password_hash) VALUES ('a@example.com', 'A', '')`)
	for _, ws := range []int64{gone, kept, 0} {
		if err := audit.Record(context.Background(), d, audit.Event{Workspace: ws, Actor: user, Action: audit.SpaceCreated}); err != nil {
			t.Fatal(err)
		}
	}
	exec(t, d, `DELETE FROM workspaces WHERE id = ?`, gone)
	if rows, _ := audit.List(context.Background(), d, audit.Filter{}); len(rows) != 2 {
		t.Fatalf("%d events left, want 2: %+v", len(rows), rows)
	}
	// A deleted actor leaves the events, without an actor.
	exec(t, d, `DELETE FROM users WHERE id = ?`, user)
	rows, _ := audit.List(context.Background(), d, audit.Filter{})
	if len(rows) != 2 || rows[0].ActorID != nil || rows[1].ActorID != nil {
		t.Fatalf("events = %+v, want 2 without an actor", rows)
	}
}

func TestListIsPerWorkspaceNewestFirstAndBounded(t *testing.T) {
	d := open(t)
	a := exec(t, d, `INSERT INTO workspaces (name) VALUES ('A')`)
	b := exec(t, d, `INSERT INTO workspaces (name) VALUES ('B')`)
	for i := 0; i < 5; i++ {
		if err := audit.Record(context.Background(), d, audit.Event{Workspace: a, Action: audit.SpaceUpdated, Target: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := audit.Record(context.Background(), d, audit.Event{Workspace: b, Action: audit.SpaceUpdated}); err != nil {
		t.Fatal(err)
	}
	rows, err := audit.List(context.Background(), d, audit.Filter{Workspace: a, Limit: 3})
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	for i, r := range rows {
		if *r.WorkspaceID != a || *r.TargetID != int64(5-i) {
			t.Fatalf("row %d = %+v: want workspace A, newest first", i, r)
		}
	}
	if rows, _ := audit.List(context.Background(), d, audit.Filter{Workspace: a, Action: audit.SpaceCreated}); len(rows) != 0 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestPruneRemovesOnlyOldEvents(t *testing.T) {
	d := open(t)
	old := time.Now().Add(-400 * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	recent := time.Now().Add(-10 * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	for _, at := range []string{old, old, recent} {
		exec(t, d, `INSERT INTO audit_events (at, action) VALUES (?, 'login_succeeded')`, at)
	}
	n, err := audit.Prune(context.Background(), d, time.Now().Add(-audit.DefaultRetention))
	if err != nil || n != 2 {
		t.Fatalf("Prune = %d, %v, want 2", n, err)
	}
	if rows, _ := audit.List(context.Background(), d, audit.Filter{}); len(rows) != 1 || rows[0].At != recent {
		t.Fatalf("left %+v", rows)
	}
}

func TestPruneWorksInBatches(t *testing.T) {
	d := open(t)
	old := time.Now().Add(-10 * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	if _, err := d.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 12000)
		INSERT INTO audit_events (at, action) SELECT ?, 'logout' FROM n`, old); err != nil {
		t.Fatal(err)
	}
	n, err := audit.Prune(context.Background(), d, time.Now())
	if err != nil || n != 12000 {
		t.Fatalf("Prune = %d, %v, want 12000", n, err)
	}
}

func TestRetainSweepsAtStartAndStops(t *testing.T) {
	d := open(t)
	exec(t, d, `INSERT INTO audit_events (at, action) VALUES ('2000-01-01T00:00:00.000Z', 'logout')`)
	if err := audit.Record(context.Background(), d, audit.Event{Action: audit.Logout}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		audit.Retain(ctx, d, 24*time.Hour, time.Hour)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, _ := audit.List(context.Background(), d, audit.Filter{})
		if len(rows) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the old event was not swept: %+v", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Retain did not stop")
	}
}

func TestRetainOffKeepsEverything(t *testing.T) {
	d := open(t)
	exec(t, d, `INSERT INTO audit_events (at, action) VALUES ('2000-01-01T00:00:00.000Z', 'logout')`)
	done := make(chan struct{})
	go func() {
		audit.Retain(context.Background(), d, 0, time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Retain with no limit should return at once")
	}
	if rows, _ := audit.List(context.Background(), d, audit.Filter{}); len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.example.com/v1":           "api.example.com",
		"https://user:pass@api.example.com/v1": "api.example.com",
		"http://127.0.0.1:11434/v1?key=secret": "127.0.0.1:11434",
		"":                                     "",
		"not a url":                            "",
		"/relative/path":                       "",
	} {
		if got := audit.Host(in); got != want {
			t.Errorf("Host(%q) = %q, want %q", in, got, want)
		}
	}
}

// probe records what the helper reports.
type probe struct {
	testing.TB
	errors []string
}

func (p *probe) Helper()                   {}
func (p *probe) Errorf(f string, a ...any) { p.errors = append(p.errors, fmt.Sprintf(f, a...)) }
func (p *probe) Fatalf(f string, a ...any) { p.errors = append(p.errors, fmt.Sprintf(f, a...)) }

func TestNoSecretsFindsOne(t *testing.T) {
	d := open(t)
	if err := audit.Record(context.Background(), d, audit.Event{Action: audit.SpaceCreated,
		Details: audit.Details{"name": "contains-fake-secret-value"}}); err != nil {
		t.Fatal(err)
	}
	var none probe
	audittest.NoSecrets(&none, d, map[string]string{"an unrelated value": "some-other-fake-value"})
	if len(none.errors) != 0 {
		t.Fatalf("reported %v for a value that isn't there", none.errors)
	}
	var found probe
	audittest.NoSecrets(&found, d, map[string]string{"a known secret": "fake-secret-value"})
	if len(found.errors) != 1 {
		t.Fatalf("reported %v, want one error for the secret in the row", found.errors)
	}
	var short probe
	audittest.NoSecrets(&short, d, map[string]string{"a short probe": "abc"})
	if len(short.errors) != 1 {
		t.Fatalf("a probe of 3 characters was accepted: %v", short.errors)
	}
}
