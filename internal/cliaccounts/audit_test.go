//go:build unix

package cliaccounts

import (
	"context"
	"fmt"
	"net/http"
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
		c.Details["cli"] != "claude" || c.Details["label"] != "Company key" || c.Details["api_key_set"] != true {
		t.Fatalf("created = %+v", c)
	}

	path := sharedPath(ws) + "/" + itoa(a.ID)
	expect(t, admin.do(http.MethodPatch, path, map[string]string{"label": "Renamed", "api_key": "fake-other-key-that-fails"}), http.StatusOK)
	updated := e.events(ws, audit.SharedCLIAccountUpdated)
	if len(updated) != 1 || *updated[0].ActorID != admin.id || updated[0].Details["api_key_changed"] != true ||
		fmt.Sprint(updated[0].Details["fields"]) != "[api_key label]" || updated[0].Details["label"] != "Renamed" {
		t.Fatalf("updated = %+v", updated)
	}

	expect(t, admin.do(http.MethodPatch, sharedPath(ws)+"/modes", map[string]any{"default_mode": "shared", "members": map[string]any{itoa(member.id): "own"}}), http.StatusOK)
	modes := e.events(ws, audit.CLIModesChanged)
	if len(modes) != 1 || *modes[0].ActorID != admin.id || modes[0].Details["default_mode"] != "shared" || modes[0].Details["members_changed"] != float64(1) {
		t.Fatalf("modes = %+v", modes)
	}

	expect(t, admin.do(http.MethodDelete, path, nil), http.StatusNoContent)
	removed := e.events(ws, audit.SharedCLIAccountRemoved)
	if len(removed) != 1 || *removed[0].TargetID != a.ID || *removed[0].ActorID != admin.id || removed[0].Details["label"] != "Renamed" {
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
