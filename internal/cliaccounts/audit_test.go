//go:build unix

package cliaccounts

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/audit/audittest"
	"taskto.ai/taskto/internal/cliworker"
)

func (e *env) events(workspace int64, action string) []audit.Row {
	e.t.Helper()
	rows, err := audit.List(context.Background(), e.db, audit.Filter{Workspace: workspace, Action: action, Limit: 100})
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func TestAuditSharedCLIAccounts(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	admin, ws := e.setup()
	member := e.invite(admin, ws, "member@example.com")
	p := newProvider(t)

	a := admin.addShared(ws, cliworker.Claude, p.URL+"/v1/")
	created := e.events(ws, audit.SharedCLIAccountCreated)
	if len(created) != 1 {
		t.Fatalf("shared_cli_account_created = %+v", created)
	}
	c := created[0]
	if *c.ActorID != admin.id || *c.TargetID != a.ID || c.TargetType != audit.TargetSharedAccount ||
		c.Details["cli"] != "claude" || c.Details["label"] != nil || c.Details["model"] != nil || c.Details["api_key_set"] != true ||
		c.TargetCreatedAt == nil {
		t.Fatalf("created = %+v", c)
	}

	path := sharedPath(ws) + "/" + itoa(a.ID)
	expect(t, admin.do(http.MethodPatch, path, map[string]string{"label": "Renamed", "api_key": "fake-other-key-that-fails"}), http.StatusOK)
	updated := e.events(ws, audit.SharedCLIAccountUpdated)
	if len(updated) != 1 || *updated[0].ActorID != admin.id || updated[0].Details["api_key_changed"] != true ||
		fmt.Sprint(updated[0].Details["fields"]) != "[api_key label]" || updated[0].Details["label"] != nil || updated[0].TargetCreatedAt == nil {
		t.Fatalf("updated = %+v", updated)
	}

	expect(t, admin.do(http.MethodPatch, sharedPath(ws)+"/modes", map[string]any{"default_mode": "shared", "members": map[string]any{itoa(member.id): "own"}}), http.StatusOK)
	modes := e.events(ws, audit.CLIModesChanged)
	if len(modes) != 1 || *modes[0].ActorID != admin.id || modes[0].Details["default_mode"] != "shared" || modes[0].Details["members_changed"] != float64(1) {
		t.Fatalf("modes = %+v", modes)
	}

	expect(t, admin.do(http.MethodDelete, path, nil), http.StatusNoContent)
	removed := e.events(ws, audit.SharedCLIAccountRemoved)
	if len(removed) != 1 || *removed[0].TargetID != a.ID || *removed[0].ActorID != admin.id || removed[0].Details["label"] != nil || removed[0].TargetCreatedAt == nil ||
		*removed[0].TargetCreatedAt != *created[0].TargetCreatedAt {
		t.Fatalf("removed = %+v", removed)
	}

	audittest.NoSecrets(t, e.db, map[string]string{"shared key": sharedKey, "rotated key": "fake-other-key-that-fails"})
}

func TestAuditSharedCLIAccountRefusalsRecordNothing(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	admin, ws := e.setup()
	member := e.invite(admin, ws, "member@example.com")
	outsider := e.invite(admin, ws, "outsider@example.com")
	if _, err := e.db.Exec(`DELETE FROM memberships WHERE user_id = ?`, outsider.id); err != nil {
		t.Fatal(err)
	}
	other := e.secondWorkspace(outsider)
	a := admin.addShared(ws, cliworker.Claude, newProvider(t).URL)
	n := len(e.events(0, ""))

	path := sharedPath(ws) + "/" + itoa(a.ID)
	expect(t, member.do(http.MethodPost, sharedPath(ws), map[string]string{"cli": "grok", "label": "x", "api_key": sharedKey}), http.StatusForbidden)
	expect(t, member.do(http.MethodPatch, path, map[string]string{"label": "mine"}), http.StatusForbidden)
	expect(t, member.do(http.MethodDelete, path, nil), http.StatusForbidden)
	expect(t, member.do(http.MethodPatch, sharedPath(ws)+"/modes", map[string]any{"default_mode": "shared"}), http.StatusForbidden)
	otherPath := sharedPath(other) + "/" + itoa(a.ID)
	expect(t, outsider.do(http.MethodPatch, otherPath, map[string]string{"label": "stolen"}), http.StatusNotFound)
	expect(t, outsider.do(http.MethodDelete, otherPath, nil), http.StatusNotFound)
	expect(t, admin.do(http.MethodPost, sharedPath(ws), map[string]string{"cli": "claude", "label": "x", "api_key": "short"}), http.StatusBadRequest)
	expect(t, admin.do(http.MethodPatch, sharedPath(ws)+"/modes", map[string]any{"members": map[string]any{itoa(outsider.id): "own"}}), http.StatusNotFound)
	if got := len(e.events(0, "")); got != n {
		t.Fatalf("refusals left %d events", got-n)
	}
}

// staleShared returns a shared account of ws as a handler read it, and then makes it gone and
// gives its id to an account of another workspace, as a concurrent delete and create would.
func staleShared(t *testing.T, e *env, admin *client, ws, other int64) (sharedRow, int64) {
	t.Helper()
	a := admin.addShared(ws, cliworker.Claude, newProvider(t).URL)
	row, ok, err := e.svc.sharedAccount(context.Background(), ws, a.ID)
	if err != nil || !ok {
		t.Fatalf("sharedAccount = %v, %v", ok, err)
	}
	if _, err := e.db.Exec(`DELETE FROM cli_shared_accounts WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO cli_shared_accounts (id, workspace_id, cli, label, base_url) VALUES (?, ?, 'codex', 'Theirs', 'https://api.openai.com')`,
		a.ID, other); err != nil {
		t.Fatal(err)
	}
	return row, a.ID
}

func TestAuditSharedUpdateOfAReplacedAccountChangesAndRecordsNothing(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	admin, ws := e.setup()
	other := e.secondWorkspace(admin)
	row, id := staleShared(t, e, admin, ws, other)
	n := len(e.events(0, ""))

	req := httptest.NewRequest(http.MethodPatch, sharedPath(ws)+"/"+itoa(id), strings.NewReader(`{"label": "Hijacked"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.svc.handleSharedUpdate(rec, req, row)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update of a replaced account = %d, want 404", rec.Code)
	}
	var label string
	if err := e.db.QueryRow(`SELECT label FROM cli_shared_accounts WHERE id = ?`, id).Scan(&label); err != nil || label != "Theirs" {
		t.Fatalf("the other workspace's account is %q, %v", label, err)
	}
	if got := len(e.events(0, "")); got != n {
		t.Fatalf("the update recorded %d events", got-n)
	}
}

func TestAuditSharedDeleteOfAReplacedAccountDeletesAndRecordsNothing(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	admin, ws := e.setup()
	other := e.secondWorkspace(admin)
	row, id := staleShared(t, e, admin, ws, other)
	n := len(e.events(0, ""))

	rec := httptest.NewRecorder()
	e.svc.handleSharedDelete(rec, httptest.NewRequest(http.MethodDelete, sharedPath(ws)+"/"+itoa(id), nil), row)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete of a replaced account = %d, want 404", rec.Code)
	}
	var count int
	if err := e.db.QueryRow(`SELECT count(*) FROM cli_shared_accounts WHERE id = ? AND workspace_id = ?`, id, other).Scan(&count); err != nil || count != 1 {
		t.Fatalf("the other workspace's account was deleted: %d, %v", count, err)
	}
	if got := len(e.events(0, "")); got != n {
		t.Fatalf("the delete recorded %d events", got-n)
	}
}

func TestAuditTwoDeletesOfOneSharedAccountMakeOneRemoval(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	admin, ws := e.setup()
	a := admin.addShared(ws, cliworker.Claude, newProvider(t).URL)
	row, _, err := e.svc.sharedAccount(context.Background(), ws, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Both deletes read the row before either wrote: the second finds it gone.
	codes := []int{}
	for range 2 {
		rec := httptest.NewRecorder()
		e.svc.handleSharedDelete(rec, httptest.NewRequest(http.MethodDelete, sharedPath(ws)+"/"+itoa(a.ID), nil), row)
		codes = append(codes, rec.Code)
	}
	if codes[0] != http.StatusNoContent || codes[1] != http.StatusNotFound {
		t.Fatalf("codes = %v, want 204 then 404", codes)
	}
	if got := len(e.events(ws, audit.SharedCLIAccountRemoved)); got != 1 {
		t.Fatalf("shared_cli_account_removed = %d events, want 1", got)
	}
}

func TestAuditCLIModesThatChangeNothingRecordNothing(t *testing.T) {
	e := newEnv(t, LocalUnsafe, nil)
	admin, ws := e.setup()
	member := e.invite(admin, ws, "member@example.com")
	path := sharedPath(ws) + "/modes"

	// Empty, and a request that sets what already is (the default, and a member with no mode of
	// their own given the default again).
	expect(t, admin.do(http.MethodPatch, path, map[string]any{}), http.StatusOK)
	expect(t, admin.do(http.MethodPatch, path, map[string]any{"default_mode": "own_else_shared", "members": map[string]any{itoa(member.id): nil}}), http.StatusOK)
	if got := len(e.events(ws, audit.CLIModesChanged)); got != 0 {
		t.Fatalf("cli_account_modes_changed = %d events for requests that changed nothing", got)
	}

	expect(t, admin.do(http.MethodPatch, path, map[string]any{"default_mode": "own", "members": map[string]any{itoa(member.id): "shared"}}), http.StatusOK)
	expect(t, admin.do(http.MethodPatch, path, map[string]any{"default_mode": "own", "members": map[string]any{itoa(member.id): "shared"}}), http.StatusOK)
	got := e.events(ws, audit.CLIModesChanged)
	if len(got) != 1 || got[0].Details["default_mode"] != "own" || got[0].Details["members_changed"] != float64(1) {
		t.Fatalf("modes = %+v, want one event for the first request", got)
	}
	// Only the member changes: no default_mode in the event.
	expect(t, admin.do(http.MethodPatch, path, map[string]any{"default_mode": "own", "members": map[string]any{itoa(member.id): nil}}), http.StatusOK)
	got = e.events(ws, audit.CLIModesChanged)
	if len(got) != 2 || got[0].Details["default_mode"] != nil || got[0].Details["members_changed"] != float64(1) {
		t.Fatalf("modes = %+v", got)
	}
}
