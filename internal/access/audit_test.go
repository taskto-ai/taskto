package access

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/auth"
)

func (e *env) events(workspace int64, action string) []audit.Row {
	e.t.Helper()
	rows, err := audit.List(context.Background(), e.db, audit.Filter{Workspace: workspace, Action: action, Limit: 100})
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func TestAuditSpaces(t *testing.T) {
	e := newEnv(t)
	owner, ws := e.setup()
	alice := e.join(owner, ws, "Alice", auth.RoleMember)

	// Creating a space.
	s := e.createShared(owner, ws, "Team")
	rows := e.events(ws, audit.SpaceCreated)
	if len(rows) != 1 {
		t.Fatalf("space_created = %d events, want 1", len(rows))
	}
	r := rows[0]
	if *r.ActorID != owner.id || *r.TargetID != s.ID || r.TargetType != audit.TargetSpace || r.Details["name"] != nil {
		t.Fatalf("space_created = %+v", r)
	}

	// Adding a member, changing their role, removing them.
	e.addMember(owner, s, alice, RoleViewer)
	added := e.events(ws, audit.SpaceMemberAdded)
	if len(added) != 1 || *added[0].ActorID != owner.id || *added[0].TargetID != s.ID ||
		added[0].Details["user_id"] != float64(alice.id) || added[0].Details["role"] != "viewer" {
		t.Fatalf("space_member_added = %+v", added)
	}
	expect(t, owner.do(http.MethodPatch, spacePath(s, memberPath(alice)), map[string]any{"role": RoleEditor}), http.StatusNoContent)
	changed := e.events(ws, audit.SpaceMemberRoleChanged)
	if len(changed) != 1 || changed[0].Details["from"] != "viewer" || changed[0].Details["to"] != "editor" ||
		changed[0].Details["user_id"] != float64(alice.id) {
		t.Fatalf("space_member_role_changed = %+v", changed)
	}
	expect(t, owner.do(http.MethodDelete, spacePath(s, memberPath(alice)), nil), http.StatusNoContent)
	removed := e.events(ws, audit.SpaceMemberRemoved)
	if len(removed) != 1 || removed[0].Details["role"] != "editor" || removed[0].Details["user_id"] != float64(alice.id) {
		t.Fatalf("space_member_removed = %+v", removed)
	}

	// Changing the space: the fields are named, and the values are the non-secret ones.
	expect(t, owner.do(http.MethodPatch, spacePath(s, ""), map[string]any{"name": "Crew", "archived": true, "offline_downloads": true}), http.StatusOK)
	updated := e.events(ws, audit.SpaceUpdated)
	if len(updated) != 1 {
		t.Fatalf("space_updated = %+v", updated)
	}
	d := updated[0].Details
	if d["name"] != nil || d["archived"] != true || d["offline_downloads"] != true || fmt.Sprint(d["fields"]) != "[name archived offline_downloads]" {
		t.Fatalf("details = %v", d)
	}
}

func TestAuditSpaceRefusalsRecordNothing(t *testing.T) {
	e := newEnv(t)
	owner, ws := e.setup()
	alice := e.join(owner, ws, "Alice", auth.RoleMember)
	bob := e.join(owner, ws, "Bob", auth.RoleMember)
	s := e.createShared(owner, ws, "Team")

	// A member who isn't an admin changes nothing.
	expect(t, alice.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces", ws), map[string]string{"name": "Mine"}), http.StatusForbidden)
	expect(t, alice.do(http.MethodPatch, spacePath(s, ""), map[string]any{"name": "Mine"}), http.StatusForbidden)
	expect(t, alice.do(http.MethodPost, spacePath(s, "/members"), map[string]any{"user_id": alice.id, "role": RoleEditor}), http.StatusForbidden)

	// An admin of another workspace can't reach this one's space.
	stranger := e.join(owner, ws, "Stranger", auth.RoleMember)
	other := e.addWorkspace(stranger, "Elsewhere")
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, stranger.id), nil), http.StatusNoContent)
	before := len(e.events(0, ""))
	expect(t, stranger.do(http.MethodPatch, spacePath(in(other, s), ""), map[string]any{"name": "Taken"}), http.StatusNotFound)
	expect(t, stranger.do(http.MethodPost, spacePath(in(other, s), "/members"), map[string]any{"user_id": stranger.id, "role": RoleEditor}), http.StatusNotFound)

	// A failed change: a member twice, and one who isn't in the workspace.
	e.addMember(owner, s, bob, RoleViewer)
	afterFirst := len(e.events(0, ""))
	expect(t, owner.do(http.MethodPost, spacePath(s, "/members"), map[string]any{"user_id": bob.id, "role": RoleViewer}), http.StatusConflict)
	expect(t, owner.do(http.MethodPost, spacePath(s, "/members"), map[string]any{"user_id": stranger.id, "role": RoleViewer}), http.StatusNotFound)
	expect(t, owner.do(http.MethodDelete, spacePath(s, memberPath(alice)), nil), http.StatusNotFound)
	if got := len(e.events(0, "")); got != afterFirst {
		t.Fatalf("refused changes left %d events", got-afterFirst)
	}
	if afterFirst != before+1 {
		t.Fatalf("one member added: %d events, want %d", afterFirst, before+1)
	}

	// The other workspace's log has none of this workspace's events, and the reverse.
	for _, r := range e.events(other, "") {
		if r.Action == audit.SpaceUpdated || r.Action == audit.SpaceMemberAdded {
			t.Fatalf("the other workspace's log holds %+v", r)
		}
	}
}
