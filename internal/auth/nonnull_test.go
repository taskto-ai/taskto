package auth

import (
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/apitypes/nonnull"
)

// The reply of /api/me tells the workspaces as a list, never null (apitypes:"nonnull"), even
// for someone who is in none.
func TestMeWorkspacesAreNeverNil(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	got := decode[Me](t, owner.do(http.MethodGet, "/api/me", nil))
	nonnull.Require(t, got)
	if len(got.Workspaces) != 1 {
		t.Fatalf("workspaces: %+v", got.Workspaces)
	}

	// Left in none: an empty list.
	if _, err := e.db.Exec(`DELETE FROM memberships WHERE workspace_id = ?`, ws); err != nil {
		t.Fatal(err)
	}
	got = decode[Me](t, owner.do(http.MethodGet, "/api/me", nil))
	if len(got.Workspaces) != 0 {
		t.Fatalf("workspaces: %+v", got.Workspaces)
	}
	nonnull.Require(t, got)

	// A login says it too.
	rec := e.client().do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": ownerPassword})
	expect(t, rec, http.StatusOK)
	nonnull.Require(t, decode[Me](t, rec))
}
