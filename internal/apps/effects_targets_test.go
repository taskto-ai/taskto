package apps

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/access"
)

// The draft the builder's panel reads comes with the names of the spaces and boards its effects
// act on, and only the names the caller may read.
func TestTheDraftNamesItsEffectTargetsOnlyToWhoMayReadThem(t *testing.T) {
	f := newFX(t)
	want := []EffectTarget{
		{SpaceID: f.maint.ID, Space: "Maintenance", SpaceKind: access.Shared, BoardID: f.board, Board: "Repairs"},
		{SpaceID: f.maint.ID, Space: "Maintenance", SpaceKind: access.Shared},
	}
	// Each target once: the card's board and the document's space, which the route and the
	// moves of the card don't add to.
	if got := f.view(f.alice, f.shared, f.app).EffectTargets; !slices.Equal(got, want) {
		t.Fatalf("a reader of the space: %+v", got)
	}

	// Bob reads the space but can't design the app: he is told of no draft, and so of no target.
	rec := f.bob.do(http.MethodGet, appPath(f.shared, f.app.ID, ""), nil)
	expect(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "effect_targets") || strings.Contains(rec.Body.String(), "Repairs") {
		t.Fatalf("a caller who can't see the draft is told its targets: %s", rec.Body.String())
	}

	// Alice leaves the Maintenance space: she still designs the app, and is told the targets'
	// IDs, which are in its definition anyway, and no name.
	expect(t, f.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", f.ws, f.maint.ID, f.alice.id), nil), http.StatusNoContent)
	rec = f.alice.do(http.MethodGet, appPath(f.shared, f.app.ID, ""), nil)
	expect(t, rec, http.StatusOK)
	got := decode[App](t, rec).EffectTargets
	if len(got) != 2 || got[0] != (EffectTarget{SpaceID: f.maint.ID, BoardID: f.board}) || got[1] != (EffectTarget{SpaceID: f.maint.ID}) {
		t.Fatalf("a caller who can't read the space: %+v", got)
	}
	if strings.Contains(rec.Body.String(), "Maintenance") || strings.Contains(rec.Body.String(), "Repairs") {
		t.Fatalf("names of what the caller can't read: %s", rec.Body.String())
	}
}

// A target whose board is archived is told by its IDs alone: nothing of its space, as the API
// says for a board that isn't one of the space's.
func TestAnArchivedBoardIsAnEffectTargetByItsIDsAlone(t *testing.T) {
	f := newFX(t)
	if _, err := f.e.db.Exec(`UPDATE boards SET archived_at = CURRENT_TIMESTAMP WHERE id = ?`, f.board); err != nil {
		t.Fatal(err)
	}
	got := f.view(f.alice, f.shared, f.app).EffectTargets
	want := []EffectTarget{
		{SpaceID: f.maint.ID, BoardID: f.board},
		{SpaceID: f.maint.ID, Space: "Maintenance", SpaceKind: access.Shared},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("targets with an archived board: %+v", got)
	}
}
