package search

import (
	"errors"
	"math/rand/v2"
	"testing"

	"taskto.ai/taskto/internal/access"
)

// generationOf is sp with the generation its row has now, as the access check reads it.
func (w *world) generationOf(sp access.Space) access.Space {
	w.e.t.Helper()
	if err := w.e.db.QueryRow(`SELECT generation FROM spaces WHERE id = ?`, sp.ID).Scan(&sp.Generation); err != nil || sp.Generation == 0 {
		w.e.t.Fatalf("the generation of space %d: %d, %v", sp.ID, sp.Generation, err)
	}
	return sp
}

func (w *world) exec(query string, args ...any) {
	w.e.t.Helper()
	if _, err := w.e.db.Exec(query, args...); err != nil {
		w.e.t.Fatalf("%s: %v", query, err)
	}
}

// replace removes sp as an account's purge does (its files, then its rows) and makes another
// space with its ID, in the same workspace; the new one has the consent the old one had. It returns
// the new space, as the access check reads it.
func (w *world) replace(sp access.Space) access.Space {
	w.e.t.Helper()
	var account int64
	var model, baseURL string
	_ = w.e.db.QueryRow(`SELECT account_id, model, base_url FROM search_embedding_consent WHERE space_id = ?`, sp.ID).Scan(&account, &model, &baseURL)
	if err := w.e.idx.Knowledge().RemoveSpace(sp); err != nil {
		w.e.t.Fatal(err)
	}
	w.exec(`DELETE FROM spaces WHERE id = ?`, sp.ID)
	w.exec(`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (?, ?, 'shared', 'Replacement')`, sp.ID, sp.WorkspaceID)
	if account != 0 {
		w.exec(`INSERT INTO search_embedding_consent (space_id, account_id, model, base_url) VALUES (?, ?, ?, ?)`, sp.ID, account, model, baseURL)
	}
	fresh := w.generationOf(access.Space{ID: sp.ID, WorkspaceID: sp.WorkspaceID, Kind: sp.Kind, Name: "Replacement", Access: access.Write})
	if fresh.Generation == sp.Generation {
		w.e.t.Fatalf("fixture: the replacement has the generation %d too", sp.Generation)
	}
	return fresh
}

// A sync that read a space's files, and waits for its transaction while the space is removed and
// another made with its ID, must not write what it read into the index of the new space: the index's
// rows are by the ID, which the foreign keys take for the new space's.
func TestASyncPausedAcrossARemovalAndReuseIndexesNothingInTheReplacement(t *testing.T) {
	w := newWorld(t)
	old := w.generationOf(w.createShared("Gone"))
	w.put(w.owner, old, "a.md", "a kumquat in the old space")
	w.put(w.owner, old, "b.md", "another kumquat")

	var replacement access.Space
	w.e.idx.onBatch = func(int, int) { // the sync has loaded both files and has not begun its transaction
		w.e.idx.onBatch = nil
		replacement = w.replace(old)
	}
	_, err := w.e.idx.Sync(ctx, old)
	if !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("a sync of a space that was replaced: %v", err)
	}
	for _, q := range []string{`SELECT count(*) FROM search_docs WHERE space_id = ?`, `SELECT count(*) FROM search_spaces WHERE space_id = ?`} {
		var n int
		if err := w.e.db.QueryRow(q, replacement.ID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: %d, %v; the replacement has the old space's documents in the index", q, n, err)
		}
	}
}

// A search authorized for one space reads the index of that space only: when the space is removed
// and another made with its ID while the search is in flight, what it finds of the new one is
// nothing, in the full-text ranking and in the semantic ones that hybrid search fuses.
func TestASearchAuthorizedForAReplacedSpaceFindsNothingOfTheReplacement(t *testing.T) {
	w := newWorld(t)
	account := w.setModel() // and consents to it for the team space
	key := Embeddings{Account: account, Model: fakeModel}.key()
	old := w.generationOf(w.team)
	old.Access = access.Read // what the reader's access check found

	fresh := w.replace(old)
	w.e.idx.Knowledge().SetSpaceCheck(access.SpaceRowCheck(w.e.db))
	if _, err := w.e.idx.Knowledge().Put(ctx, fresh, "fruit.md", []byte("a kumquat in the replacement"), "", by, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.idx.Sync(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	docs := w.fillVectors(fresh, key, 4, 2, fakeDims, 1)
	qv := randomVector(rand.New(rand.NewPCG(9, 9)), fakeDims)
	staleIn, freshIn := spacesJSON(t, old), spacesJSON(t, fresh)
	match := matchExpr("kumquat")

	// What the replacement's own readers find, to show the fixture finds it.
	if hits := must[[]hit](t)(w.e.idx.fullText(ctx, freshIn, match, 10, 0, false)); len(hits) != 1 {
		t.Fatalf("fixture: the replacement's own search finds %d documents", len(hits))
	}
	if found := must[[]near](t)(w.e.idx.nearest(ctx, w.ws, freshIn, key, qv, 10)); len(found) == 0 {
		t.Fatal("fixture: the replacement has no vectors to find")
	}

	res := must[Results](t)(w.e.idx.Search(ctx, w.ws, []access.Space{old}, Query{Text: "kumquat"}))
	if len(res.Results) != 0 {
		t.Fatalf("a search authorized for the old space found %+v of the replacement", res.Results)
	}
	if hits := must[[]hit](t)(w.e.idx.fullText(ctx, staleIn, match, 10, 0, false)); len(hits) != 0 {
		t.Fatalf("the full-text ranking under the old authorization found %d documents of the replacement", len(hits))
	}
	if found := must[[]near](t)(w.e.idx.nearest(ctx, w.ws, staleIn, key, qv, 10)); len(found) != 0 {
		t.Fatalf("the semantic ranking under the old authorization found %d passages of the replacement", len(found))
	}
	if found := must[[]near](t)(w.e.idx.nearestBrute(ctx, staleIn, key, qv)); len(found) != 0 {
		t.Fatalf("the uncached semantic ranking under the old authorization found %d passages of the replacement", len(found))
	}
	fused := must[[]hit](t)(w.e.idx.fuse(ctx, staleIn, nil, []near{{doc: docs[0]}}))
	if len(fused) != 0 {
		t.Fatalf("the fusion under the old authorization named %d documents of the replacement", len(fused))
	}
}
