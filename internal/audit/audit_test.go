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
	err := audit.Record(ctx, d, audit.Event{Workspace: ws, Action: audit.SpaceUpdated, TargetType: audit.TargetSpace, Target: 7,
		Details: audit.Details{"archived": false, "fields": []string{"name", "archived"}, "offline_downloads": true}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := audit.List(context.Background(), d, audit.Filter{Workspace: ws})
	if err != nil || len(rows) != 1 {
		t.Fatalf("List = %+v, %v", rows, err)
	}
	r := rows[0]
	if *r.WorkspaceID != ws || *r.ActorID != user || r.Action != "space_updated" || r.TargetType != "space" || *r.TargetID != 7 ||
		*r.ClientAddr != "203.0.113.9" || r.Details["offline_downloads"] != true || r.Details["archived"] != false {
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
		{"an action without a schema", "made_up_action", nil},
		{"a password", audit.PasswordChanged, audit.Details{"password": "hunter2-hunter2"}},
		{"a token", audit.AccessTokenCreated, audit.Details{"token": "ttp_abc"}},
		{"a token's name", audit.AccessTokenCreated, audit.Details{"name": "ttp_abcdefghijklmnop"}},
		{"a token's name when revoked", audit.AccessTokenRevoked, audit.Details{"name": "laptop"}},
		{"a space's name", audit.SpaceCreated, audit.Details{"name": "Team"}},
		{"a label", audit.SharedCLIAccountCreated, audit.Details{"label": "Company key"}},
		{"a model", audit.SearchEmbeddingsChanged, audit.Details{"model": "text-embedding-3-small"}},
		{"a key's value", audit.ProviderAccountCreated, audit.Details{"api_key": "sk-123"}},
		{"a key's value under its boolean's name", audit.ProviderAccountCreated, audit.Details{"api_key_set": "sk-123"}},
		{"a reset link", audit.PasswordResetCreated, audit.Details{"reset_link": "https://x/y"}},
		{"a session", audit.Logout, audit.Details{"session_id": "abc"}},
		{"a list of anything but fields", audit.SpaceUpdated, audit.Details{"fields": []string{"password"}}},
		{"a nested value", audit.InviteCreated, audit.Details{"role": map[string]string{"name": "x"}}},
		{"a byte slice", audit.InviteCreated, audit.Details{"email": []byte("a@example.com")}},
		{"a role that isn't one", audit.InviteCreated, audit.Details{"role": "ttp_abcdefghijklmnop"}},
		{"an email that is a token", audit.InviteCreated, audit.Details{"email": "ttp_abcdefghijklmnop"}},
		{"a host with a path", audit.ProviderAccountCreated, audit.Details{"base_host": "example.com/v1?key=abc"}},
		{"a tag with spaces", audit.SSOLoginFailed, audit.Details{"reason": "a password is here"}},
		{"a number as a string", audit.SpaceMemberAdded, audit.Details{"user_id": "7"}},
		{"a string as a boolean", audit.InviteAccepted, audit.Details{"already_member": "yes"}},
	}
	for _, c := range bad {
		if err := audit.Record(context.Background(), d, audit.Event{Action: c.action, Details: c.details}); err == nil {
			t.Errorf("%s: recorded", c.name)
		}
	}
	if rows, _ := audit.List(context.Background(), d, audit.Filter{}); len(rows) != 0 {
		t.Fatalf("%d events were stored: %+v", len(rows), rows)
	}

	if err := audit.Record(context.Background(), d, audit.Event{Action: audit.ProviderAccountUpdated,
		Details: audit.Details{"api_key_changed": true}}); err != nil {
		t.Fatal(err)
	}
}

func TestEveryActionHasASchemaWithNoSecretText(t *testing.T) {
	d := open(t)
	for _, action := range audit.Actions() {
		if err := audit.Record(context.Background(), d, audit.Event{Action: action}); err != nil {
			t.Errorf("%s: %v", action, err)
		}
	}
	for action, schema := range audit.Schemas() {
		for name, boolean := range schema {
			if audit.SecretName(name) && !boolean {
				t.Errorf("%s: detail %q is named like a secret and isn't a boolean", action, name)
			}
		}
	}
}

func TestTheValuesOfATextKindAreChecked(t *testing.T) {
	d := open(t)
	good := []audit.Event{
		{Action: audit.InviteCreated, Details: audit.Details{"email": "someone+tag@example.com", "role": "admin"}},
		{Action: audit.SSOLoginFailed, Details: audit.Details{"provider": "google", "reason": "sso_no_account"}},
		{Action: audit.ProviderAccountCreated, Details: audit.Details{"base_host": "llm.example.com:8443", "kind": "openai-compatible"}},
		{Action: audit.InviteRevoked, Details: audit.Details{"email": strings.Repeat("a", 64) + "@" + strings.Repeat("b", 189), "role": "member"}},
		{Action: audit.SpaceUpdated, Details: audit.Details{"fields": []string{"name"}}},
		{Action: audit.SpaceMemberAdded, Details: audit.Details{"user_id": 7, "role": "editor"}},
	}
	for _, e := range good {
		if err := audit.Record(context.Background(), d, e); err != nil {
			t.Errorf("%+v: %v", e, err)
		}
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

type probe struct {
	testing.TB
	errors []string
}

func (p *probe) Helper()                   {}
func (p *probe) Errorf(f string, a ...any) { p.errors = append(p.errors, fmt.Sprintf(f, a...)) }
func (p *probe) Fatalf(f string, a ...any) { p.errors = append(p.errors, fmt.Sprintf(f, a...)) }

func TestNoSecretsFindsOne(t *testing.T) {
	d := open(t)
	if err := audit.Record(context.Background(), d, audit.Event{Action: audit.InviteCreated,
		Details: audit.Details{"email": "contains-fake-secret-value@example.com"}}); err != nil {
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

func TestAnEventKnowsWhichObjectItIsAbout(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	exec(t, d, `INSERT INTO workspaces (name, created_at) VALUES ('Home', '2026-01-01T00:00:00.000Z')`)
	first := exec(t, d, `INSERT INTO users (email, name, password_hash, created_at) VALUES ('a@example.com', 'A', '', '2026-01-02T00:00:00.000Z')`)
	if err := audit.Record(ctx, d, audit.Event{Action: audit.LoginSucceeded, TargetType: audit.TargetUser, Target: first}); err != nil {
		t.Fatal(err)
	}
	exec(t, d, `DELETE FROM users WHERE id = ?`, first)
	second := exec(t, d, `INSERT INTO users (email, name, password_hash, created_at) VALUES ('b@example.com', 'B', '', '2026-03-04T00:00:00.000Z')`)
	if second != first {
		t.Fatalf("the test needs the id to be reused: %d, then %d", first, second)
	}
	if err := audit.Record(ctx, d, audit.Event{Action: audit.LoginSucceeded, TargetType: audit.TargetUser, Target: second}); err != nil {
		t.Fatal(err)
	}
	if err := audit.Record(ctx, d, audit.Event{Action: audit.InviteRevoked, TargetType: audit.TargetInvite, Target: 9,
		TargetCreated: "2026-05-06T00:00:00.000Z"}); err != nil {
		t.Fatal(err)
	}
	rows, _ := audit.List(ctx, d, audit.Filter{Limit: 10})
	var times []string
	for _, r := range rows {
		if r.TargetCreatedAt == nil {
			t.Fatalf("%+v has no creation time", r)
		}
		times = append(times, *r.TargetCreatedAt)
	}
	want := []string{"2026-05-06T00:00:00.000Z", "2026-03-04T00:00:00.000Z", "2026-01-02T00:00:00.000Z"}
	if fmt.Sprint(times) != fmt.Sprint(want) {
		t.Fatalf("creation times = %v, want %v", times, want)
	}
}

func TestNoSecretsSeesThroughJSONEscaping(t *testing.T) {
	d := open(t)
	if _, err := d.Exec(`INSERT INTO audit_events (action, details) VALUES ('invite_created', ?)`,
		`{"email":"fake\u0026secret-value@example.com"}`); err != nil {
		t.Fatal(err)
	}
	var found probe
	audittest.NoSecrets(&found, d, map[string]string{"an escaped secret": "fake&secret-value"})
	if len(found.errors) != 1 {
		t.Fatalf("reported %v, want one error for the escaped secret", found.errors)
	}
}

func TestEveryTargetTypeHasATableWithCreationTimes(t *testing.T) {
	d := open(t)
	for _, typ := range audit.TargetTypes() {
		table := audit.TargetTable(typ)
		if _, err := d.Exec(`SELECT created_at FROM ` + table + ` WHERE id = 0`); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
}
