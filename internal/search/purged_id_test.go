package search

import (
	"context"
	"testing"

	"taskto.ai/taskto/internal/access"
)

// A space made after a purge may be given the purged space's ID, and the knowledge base refuses a
// call that carries no creation time while it still marks that ID removed. The index's sync keeps
// the creation time of the space it was given, and the catch-up at startup reads it, so the new
// space's documents are indexed.
func TestTheIndexSyncsASpaceThatTookAPurgedSpacesID(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	team := w.team
	if team.CreatedAt == "" {
		t.Fatal("fixture: the space has no creation time")
	}
	// The mark of an earlier space with the team's ID, before anything is written to it.
	earlier := access.Space{ID: team.ID, WorkspaceID: team.WorkspaceID, Kind: team.Kind, Access: access.Write, CreatedAt: "2000-01-01T00:00:00.000Z"}
	if err := w.e.idx.Knowledge().RemoveSpace(earlier); err != nil {
		t.Fatal(err)
	}
	w.put(w.owner, team, "fruit.md", "a kumquat in the new space")

	if _, err := w.e.idx.Sync(ctx, team); err != nil {
		t.Fatalf("syncing the new space: %v", err)
	}
	if got := w.find(team, "kumquat"); len(got.Results) != 1 {
		t.Fatalf("the new space's document is not indexed: %+v", got)
	}

	// At startup the catch-up knows the spaces by their rows alone.
	if _, err := w.e.db.Exec(`DELETE FROM search_spaces`); err != nil {
		t.Fatal(err)
	}
	if err := w.e.idx.syncAll(ctx); err != nil {
		t.Fatalf("the catch-up at startup: %v", err)
	}
}
