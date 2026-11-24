package providers

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/audit/audittest"
)

func (e *env) events(workspace int64, action string) []audit.Row {
	e.t.Helper()
	rows, err := audit.List(context.Background(), e.db, audit.Filter{Workspace: workspace, Action: action, Limit: 100})
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func TestAuditWorkspaceProviderAccounts(t *testing.T) {
	e := newEnv(t)
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com", "member")

	rec := owner.do(http.MethodPost, wsPath(ws, "providers"), account("gateway", "https://llm.example.com:8443/v1", ptr(testKey)))
	expect(t, rec, http.StatusCreated)
	id := decode[Account](t, rec).ID

	created := e.events(ws, audit.ProviderAccountCreated)
	if len(created) != 1 {
		t.Fatalf("provider_account_created = %+v", created)
	}
	c := created[0]
	if *c.ActorID != owner.id || *c.TargetID != id || c.TargetType != audit.TargetProvider ||
		c.Details["name"] != "gateway" || c.Details["scope"] != "workspace" || c.Details["api_key_set"] != true ||
		c.Details["base_host"] != "llm.example.com:8443" {
		t.Fatalf("created = %+v", c)
	}

	one := wsPath(ws, fmt.Sprintf("providers/%d", id))
	expect(t, owner.do(http.MethodPatch, one, map[string]any{"display_name": "Gateway", "api_key": otherKey}), http.StatusOK)
	updated := e.events(ws, audit.ProviderAccountUpdated)
	if len(updated) != 1 {
		t.Fatalf("provider_account_updated = %+v", updated)
	}
	u := updated[0]
	if *u.ActorID != owner.id || u.Details["api_key_changed"] != true || fmt.Sprint(u.Details["fields"]) != "[api_key display_name]" {
		t.Fatalf("updated = %+v", u)
	}

	// A change of nothing but the models names only them, and the key stays unchanged.
	expect(t, owner.do(http.MethodPatch, one, map[string]any{"models": []map[string]any{{"id": "model-z"}}}), http.StatusOK)
	updated = e.events(ws, audit.ProviderAccountUpdated)
	if len(updated) != 2 || updated[0].Details["api_key_changed"] != false || fmt.Sprint(updated[0].Details["fields"]) != "[models]" {
		t.Fatalf("updated = %+v", updated)
	}
	// Removing the key is a key change too.
	expect(t, owner.do(http.MethodPatch, one, map[string]any{"api_key": nil}), http.StatusOK)
	if got := e.events(ws, audit.ProviderAccountUpdated)[0]; got.Details["api_key_changed"] != true {
		t.Fatalf("removing the key = %+v", got)
	}

	expect(t, owner.do(http.MethodDelete, one, nil), http.StatusNoContent)
	removed := e.events(ws, audit.ProviderAccountRemoved)
	if len(removed) != 1 || *removed[0].TargetID != id || removed[0].Details["name"] != "gateway" || *removed[0].ActorID != owner.id {
		t.Fatalf("removed = %+v", removed)
	}

	// Refusals record nothing: a member, a missing account, a bad body.
	n := len(e.events(0, ""))
	expect(t, member.do(http.MethodPost, wsPath(ws, "providers"), account("mine", localURL, nil)), http.StatusForbidden)
	expect(t, owner.do(http.MethodDelete, one, nil), http.StatusNotFound)
	expect(t, owner.do(http.MethodPatch, one, map[string]any{"display_name": "x"}), http.StatusNotFound)
	expect(t, owner.do(http.MethodPost, wsPath(ws, "providers"), map[string]any{"name": ""}), http.StatusBadRequest)
	if got := len(e.events(0, "")); got != n {
		t.Fatalf("refusals left %d events", got-n)
	}

	// Neither key is anywhere in the log.
	audittest.NoSecrets(t, e.db, map[string]string{"first key": testKey, "rotated key": otherKey})
}

func TestAuditPersonalProviderAccountsBelongToNoWorkspace(t *testing.T) {
	e := newEnv(t)
	owner, ws := e.setup()
	member := e.invite(owner, ws, "member@example.com", "member")

	rec := member.do(http.MethodPost, "/api/me/providers", account("mine", localURL, ptr(testKey)))
	expect(t, rec, http.StatusCreated)
	id := decode[Account](t, rec).ID
	expect(t, member.do(http.MethodPatch, fmt.Sprintf("/api/me/providers/%d", id), map[string]any{"display_name": "Mine"}), http.StatusOK)
	expect(t, member.do(http.MethodDelete, fmt.Sprintf("/api/me/providers/%d", id), nil), http.StatusNoContent)

	for _, action := range []string{audit.ProviderAccountCreated, audit.ProviderAccountUpdated, audit.ProviderAccountRemoved} {
		rows := e.events(0, action)
		if len(rows) != 1 || rows[0].WorkspaceID != nil || *rows[0].ActorID != member.id || rows[0].Details["scope"] != "user" {
			t.Fatalf("%s = %+v, want one event of the member, in no workspace", action, rows)
		}
		// The workspace's admins, whose log it isn't, find nothing of it there.
		if got := e.events(ws, action); len(got) != 0 {
			t.Fatalf("the workspace's log holds %+v", got)
		}
	}
	audittest.NoSecrets(t, e.db, map[string]string{"personal key": testKey})
}
