package search

import (
	"errors"
	"math/rand/v2"
	"strings"
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
// space with its ID, in the same workspace; the new one has the consent the old one had if consent
// is set (written as a row, so the vector cache hasn't heard of it). It returns the new space, as
// the access check reads it.
func (w *world) replace(sp access.Space, consent bool) access.Space {
	w.e.t.Helper()
	var account int64
	var model, baseURL string
	_ = w.e.db.QueryRow(`SELECT account_id, model, base_url FROM search_embedding_consent WHERE space_id = ?`, sp.ID).Scan(&account, &model, &baseURL)
	if err := w.e.idx.Knowledge().RemoveSpace(sp); err != nil {
		w.e.t.Fatal(err)
	}
	w.exec(`DELETE FROM spaces WHERE id = ?`, sp.ID)
	w.exec(`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (?, ?, 'shared', 'Replacement')`, sp.ID, sp.WorkspaceID)
	if account != 0 && consent {
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
		replacement = w.replace(old, true)
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

	fresh := w.replace(old, true)
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

// A hit whose snippet is read after the space was removed and another made with its ID reads
// nothing of the new space: the document it was found as has no other text than its own, whichever
// space has the ID now. Documents are numbered so that no number is given twice, so the text the
// snippet statements read by that number is the text it was found with or none.
func TestSnippetsOfAnOldHitAreNeverTheReplacementsText(t *testing.T) {
	w := newWorld(t)
	old := w.generationOf(w.createShared("Gone"))
	w.put(w.owner, old, "old.md", "a kumquat of the old space")
	if _, err := w.e.idx.Sync(ctx, old); err != nil {
		t.Fatal(err)
	}
	match := matchExpr("kumquat")
	hits := must[[]hit](t)(w.e.idx.fullText(ctx, spacesJSON(t, old), match, 10, 0, false))
	if len(hits) != 1 {
		t.Fatalf("fixture: %d hits", len(hits))
	}
	semantic := []hit{{doc: hits[0].doc, seq: 0, match: "semantic"}}
	w.exec(`INSERT INTO search_chunks (doc_id, seq, text, vector) VALUES (?, 0, 'the old passage', ?)`, hits[0].doc, encodeVector([]float32{1}))

	// The space goes, and another with its ID is indexed, with a text of its own that matches too.
	fresh := w.replace(old, true)
	w.e.idx.Knowledge().SetSpaceCheck(access.SpaceRowCheck(w.e.db))
	if _, err := w.e.idx.Knowledge().Put(ctx, fresh, "new.md", []byte("a kumquat in the replacement"), "", by, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.idx.Sync(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	var newDoc int64
	if err := w.e.db.QueryRow(`SELECT id FROM search_docs WHERE space_id = ?`, fresh.ID).Scan(&newDoc); err != nil {
		t.Fatal(err)
	}
	w.exec(`INSERT INTO search_chunks (doc_id, seq, text, vector) VALUES (?, 0, 'the replacement passage', ?)`, newDoc, encodeVector([]float32{1}))
	if newDoc == hits[0].doc {
		t.Fatalf("the replacement's document has the number %d of a document the old space had", newDoc)
	}

	if err := w.e.idx.textSnippets(ctx, match, hits); err != nil {
		t.Fatal(err)
	}
	for _, seg := range hits[0].snippet {
		if strings.Contains(seg.Text, "replacement") {
			t.Fatalf("the snippet of the old hit is the replacement's text: %+v", hits[0].snippet)
		}
	}
	if err := w.e.idx.passages(ctx, semantic); err != nil {
		t.Fatal(err)
	}
	for _, seg := range semantic[0].snippet {
		if strings.Contains(seg.Text, "replacement") {
			t.Fatalf("the passage of the old hit is the replacement's: %+v", semantic[0].snippet)
		}
	}
}

// What the embeddings job sends to the endpoint is read with the consent that lets it, in one
// read: a space removed and made again with the ID after the job found its document waiting, whose
// consent is off, has nothing sent, even if the document it made has the number of the old one
// (which the numbering prevents; forced here, the identity of the read does).
func TestAnEmbeddingJobSendsNothingOfAReplacementWithoutConsent(t *testing.T) {
	for _, forced := range []bool{false, true} {
		name := "numbers never reused"
		if forced {
			name = "the number reused anyway"
		}
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.setModel() // and consents to it for the team space
			old := w.generationOf(w.team)
			w.put(w.owner, old, "old.md", "an old text about a quince")
			if _, err := w.e.idx.Sync(ctx, old); err != nil {
				t.Fatal(err)
			}
			var oldDoc int64
			if err := w.e.db.QueryRow(`SELECT id FROM search_docs WHERE space_id = ?`, old.ID).Scan(&oldDoc); err != nil {
				t.Fatal(err)
			}

			w.e.idx.afterPending = func() { // the job has its list, and has not begun
				w.e.idx.afterPending = nil
				fresh := w.replace(old, false) // no consent for the new space
				w.e.idx.Knowledge().SetSpaceCheck(access.SpaceRowCheck(w.e.db))
				if _, err := w.e.idx.Knowledge().Put(ctx, fresh, "new.md", []byte("a secret text of the replacement"), "", by, ""); err != nil {
					t.Error(err)
					return
				}
				if _, err := w.e.idx.Sync(ctx, fresh); err != nil {
					t.Error(err)
					return
				}
				if forced {
					w.exec(`UPDATE search_docs SET id = ? WHERE space_id = ?`, oldDoc, fresh.ID)
					w.exec(`UPDATE search_fts SET rowid = ? WHERE path = 'new.md'`, oldDoc)
				}
			}
			if _, err := w.e.idx.embedPending(ctx); err != nil {
				t.Fatal(err)
			}
			w.e.embed.mu.Lock()
			sent := append([]string(nil), w.e.embed.inputs...)
			w.e.embed.mu.Unlock()
			for _, in := range sent {
				if strings.Contains(in, "secret") || strings.Contains(in, "replacement") {
					t.Fatalf("the endpoint was sent the replacement's text: %q", in)
				}
			}
		})
	}
}

// The vector cache holds a space's vectors by its ID, and another space can have the ID: a search
// of the new one, which the cache hasn't heard of (the consent that makes it searchable by meaning
// is committed, and the cache told after), compares the vectors of the new space and nothing of the
// old, as brute force does.
func TestTheVectorCacheNeverComparesAReplacementWithTheOldSpacesVectors(t *testing.T) {
	w := newWorld(t)
	account := w.setModel() // and consents to it for the team space
	key := Embeddings{Account: account, Model: fakeModel}.key()
	old := w.generationOf(w.team)
	w.fillVectors(old, key, 20, 2, fakeDims, 1)
	qv := randomVector(rand.New(rand.NewPCG(7, 7)), fakeDims)
	// The cache loads the old space's vectors.
	for range 2 {
		if found := must[[]near](t)(w.e.idx.nearest(ctx, w.ws, spacesJSON(t, old), key, qv, 10)); len(found) == 0 {
			t.Fatal("fixture: the old space has no vectors to find")
		}
	}

	fresh := w.replace(old, true) // its consent is a row; the cache is not told
	w.e.idx.Knowledge().SetSpaceCheck(access.SpaceRowCheck(w.e.db))
	if _, err := w.e.idx.Knowledge().Put(ctx, fresh, "new.md", []byte("a document with no vectors"), "", by, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.idx.Sync(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	freshIn := spacesJSON(t, fresh)
	want := w.e.idx.bruteNearest(t, freshIn, key, qv, 10)
	if len(want) != 0 {
		t.Fatalf("fixture: brute force finds %d passages in the replacement", len(want))
	}
	got := must[[]near](t)(w.e.idx.nearest(ctx, w.ws, freshIn, key, qv, 10))
	if len(got) != 0 {
		t.Fatalf("the cached comparison finds %d passages in the replacement, which has none: the old space's vectors", len(got))
	}
}
