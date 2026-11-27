package search

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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
	if found := must[[]near](t)(w.e.idx.nearest(ctx, w.ws, freshIn, w.rank(key), qv, 10)); len(found) == 0 {
		t.Fatal("fixture: the replacement has no vectors to find")
	}

	res := must[Results](t)(w.e.idx.searchIndex(ctx, w.ws, []access.Space{old}, Query{Text: "kumquat"}))
	if len(res.Results) != 0 {
		t.Fatalf("a search authorized for the old space found %+v of the replacement", res.Results)
	}
	if hits := must[[]hit](t)(w.e.idx.fullText(ctx, staleIn, match, 10, 0, false)); len(hits) != 0 {
		t.Fatalf("the full-text ranking under the old authorization found %d documents of the replacement", len(hits))
	}
	if found := must[[]near](t)(w.e.idx.nearest(ctx, w.ws, staleIn, w.rank(key), qv, 10)); len(found) != 0 {
		t.Fatalf("the semantic ranking under the old authorization found %d passages of the replacement", len(found))
	}
	if found := must[[]near](t)(w.e.idx.nearestBrute(ctx, staleIn, w.rank(key), qv)); len(found) != 0 {
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
		if found := must[[]near](t)(w.e.idx.nearest(ctx, w.ws, spacesJSON(t, old), w.rank(key), qv, 10)); len(found) == 0 {
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
	want := w.e.idx.bruteNearest(t, freshIn, w.rank(key), qv, 10)
	if len(want) != 0 {
		t.Fatalf("fixture: brute force finds %d passages in the replacement", len(want))
	}
	got := must[[]near](t)(w.e.idx.nearest(ctx, w.ws, freshIn, w.rank(key), qv, 10))
	if len(got) != 0 {
		t.Fatalf("the cached comparison finds %d passages in the replacement, which has none: the old space's vectors", len(got))
	}
}

// moveEndpoint is what two settings updates do while a worker or a search is between two reads:
// the provider account's URL changes, and the space consents to the new URL.
func (w *world) moveEndpoint(account int64, space int64) {
	w.e.t.Helper()
	const moved = "https://moved.example.test/v1"
	w.exec(`UPDATE provider_accounts SET base_url = ? WHERE id = ?`, moved, account)
	w.exec(`UPDATE search_embedding_consent SET base_url = ? WHERE space_id = ?`, moved, space)
}

// The consent names a URL, and the text goes only to the URL the consent names: if the account's
// URL changes and the space consents to the new one while the embeddings job holds the old
// endpoint, nothing is sent to the old one, and nothing it returns is stored under the new consent.
func TestAnEmbeddingJobSendsAndStoresOnlyForTheURLTheConsentNames(t *testing.T) {
	setup := func(t *testing.T) (*world, int64, access.Space) {
		w := newWorld(t)
		account := w.setModel() // and consents to it for the team space
		team := w.generationOf(w.team)
		w.put(w.owner, team, "fruit.md", "a kumquat in the team space")
		if _, err := w.e.idx.Sync(ctx, team); err != nil {
			t.Fatal(err)
		}
		return w, account, team
	}
	t.Run("before sending", func(t *testing.T) {
		w, account, team := setup(t)
		w.e.idx.afterEndpoint = func() { w.e.idx.afterEndpoint = nil; w.moveEndpoint(account, team.ID) }
		if _, err := w.e.idx.embedPending(ctx); err != nil {
			t.Fatal(err)
		}
		if w.e.embed.sent("kumquat") {
			t.Fatal("the text was sent to the URL the space no longer consents to")
		}
	})
	t.Run("before storing", func(t *testing.T) {
		w, account, team := setup(t)
		w.e.idx.beforeStore = func() { w.e.idx.beforeStore = nil; w.moveEndpoint(account, team.ID) }
		if _, err := w.e.idx.embedPending(ctx); err != nil {
			t.Fatal(err)
		}
		var chunks int
		var version sql.NullString
		if err := w.e.db.QueryRow(`SELECT (SELECT count(*) FROM search_chunks), (SELECT embed_version FROM search_docs WHERE space_id = ?)`, team.ID).Scan(&chunks, &version); err != nil {
			t.Fatal(err)
		}
		if chunks != 0 || version.Valid {
			t.Fatalf("vectors of the old endpoint were stored under the new consent: %d chunks, embed_version %v", chunks, version)
		}
	})
}

// The query goes to the endpoint the search holds only while the space's consent names that very
// endpoint: its URL, and the workspace's account and model as they are now.
func TestASearchSendsItsQueryOnlyToTheEndpointTheConsentNames(t *testing.T) {
	for name, change := range map[string]func(w *world, account, space int64){
		"the URL moved": func(w *world, account, space int64) { w.moveEndpoint(account, space) },
		"the model changed": func(w *world, account, space int64) {
			w.exec(`UPDATE search_settings SET model = 'another-model' WHERE workspace_id = ?`, w.ws)
			w.exec(`UPDATE search_embedding_consent SET model = 'another-model' WHERE space_id = ?`, space)
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			account := w.setModel()
			team := w.generationOf(w.team)
			team.Access = access.Read
			w.put(w.owner, team, "fruit.md", "a document")
			w.e.idx.afterQueryEndpoint = func() { w.e.idx.afterQueryEndpoint = nil; change(w, account, team.ID) }
			if _, err := w.e.idx.searchIndex(ctx, w.ws, []access.Space{team}, Query{Text: "pomelo"}); err != nil {
				t.Fatal(err)
			}
			if w.e.embed.sent("pomelo") {
				t.Fatal("the query was sent to an endpoint the space's consent no longer names")
			}
		})
	}
}

// A request that was authorized for a space, and runs after the space was removed and another made
// with its ID, changes and reads nothing of the new one: not its consent, and no work for the
// embeddings worker.
func TestAConsentRequestForAReplacedSpaceTouchesNothingOfTheReplacement(t *testing.T) {
	w := newWorld(t)
	w.setModel()
	old := w.generationOf(w.createShared("Gone"))
	old.Role = access.RoleOwner
	fresh := w.replace(old, false)
	select { // nothing is waiting for the worker yet
	case <-w.e.idx.embedWake:
	default:
	}

	err := w.e.idx.setConsent(ctx, old, w.owner.id, true, nil, nil)
	if !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("consent given under the old space's authority: %v", err)
	}
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM search_embedding_consent WHERE space_id = ?`, fresh.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the replacement has %d consents, %v", n, err)
	}
	select {
	case <-w.e.idx.embedWake:
		t.Fatal("the embeddings worker was woken for a consent that was refused")
	default:
	}

	// The replacement has consent of its own: the old space's reads don't see it.
	w.exec(`INSERT INTO search_embedding_consent (space_id, account_id, model, base_url)
		SELECT ?, account_id, model, (SELECT base_url FROM provider_accounts WHERE id = account_id) FROM search_settings WHERE workspace_id = ?`, fresh.ID, w.ws)
	if _, err := w.e.idx.consent(ctx, old); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("the old space read the replacement's consent: %v", err)
	}
	if c := must[Consent](t)(w.e.idx.consent(ctx, fresh)); !c.Enabled {
		t.Fatalf("the replacement's own consent: %+v", c)
	}
	// And the space that is current can change its own.
	if err := w.e.idx.setConsent(ctx, fresh, w.owner.id, false, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// Two searches, for a space and for the space that took its ID, interleave around the cache's
// locks: the one for the removed space must not refresh the entry, which is the new one's, with
// what it finds (nothing), and the new one's search finds its vectors.
func TestTheVectorCacheOfOverlappingSearchesForReplacedSpacesIsTheNewOnes(t *testing.T) {
	w := newWorld(t)
	account := w.setModel()
	key := Embeddings{Account: account, Model: fakeModel}.key()
	old := w.generationOf(w.team)
	w.fillVectors(old, key, 8, 2, fakeDims, 1)
	qv := randomVector(rand.New(rand.NewPCG(5, 5)), fakeDims)
	cache := w.e.idx.vectors

	type pause struct{ reached, release chan struct{} }
	var mu sync.Mutex
	pauses := map[int64]*pause{}
	oldGen := old.Generation
	cache.beforeRefresh = func(sp spaceRef) { // each generation is paused once
		mu.Lock()
		p := pauses[sp.generation]
		delete(pauses, sp.generation)
		mu.Unlock()
		if p != nil {
			close(p.reached)
			<-p.release
		}
	}
	oldRef := spaceRef{id: old.ID, generation: oldGen}
	oldPause := &pause{make(chan struct{}), make(chan struct{})}
	pauses[oldGen] = oldPause
	type result struct {
		found []near
		err   error
	}
	oldDone := make(chan result, 1)
	go func() {
		found, _, err := cache.nearest(ctx, w.e.db, w.ws, []spaceRef{oldRef}, w.rank(key), qv)
		oldDone <- result{found, err}
	}()
	<-oldPause.reached // the search for the old space has the entry and not its refresh lock

	// The space is purged and another made with its ID, with vectors of its own.
	fresh := w.replace(old, true)
	w.fillVectors(fresh, key, 8, 2, fakeDims, 2)
	freshRef := spaceRef{id: fresh.ID, generation: fresh.Generation}
	freshPause := &pause{make(chan struct{}), make(chan struct{})}
	mu.Lock()
	pauses[fresh.Generation] = freshPause
	mu.Unlock()
	freshDone := make(chan result, 1)
	go func() {
		found, rest, err := cache.nearest(ctx, w.e.db, w.ws, []spaceRef{freshRef}, w.rank(key), qv)
		if err == nil && len(rest) != 0 { // read from SQLite instead
			found, err = w.e.idx.nearestBrute(ctx, spacesJSON(t, fresh), w.rank(key), qv)
		}
		freshDone <- result{found, err}
	}()
	<-freshPause.reached // both are at the same point, the new one's after the old one's

	close(oldPause.release) // the old one goes first
	if r := <-oldDone; r.err != nil {
		t.Fatal(r.err)
	}
	close(freshPause.release)
	r := <-freshDone
	if r.err != nil {
		t.Fatal(r.err)
	}
	want := w.e.idx.bruteNearest(t, spacesJSON(t, fresh), w.rank(key), qv, 10)
	if len(want) == 0 {
		t.Fatal("fixture: the replacement has no vectors")
	}
	if len(r.found) == 0 {
		t.Fatal("the new space's search found no vectors: the old space's search installed an empty entry under its generation")
	}
	// And the entry that is kept is the new space's: the next search is served from it.
	again, rest, err := cache.nearest(ctx, w.e.db, w.ws, []spaceRef{freshRef}, w.rank(key), qv)
	if err != nil || len(rest) != 0 || len(again) == 0 {
		t.Fatalf("the next search of the new space: %d found, %v left to SQLite, %v", len(again), rest, err)
	}
}

// rank is the endpoint a query was embedded by in these tests: the model's key at the fake
// endpoint the world's provider account names.
func (w *world) rank(key string) rankEndpoint {
	return rankEndpoint{key: key, baseURL: w.e.embed.srv.URL + "/v1"}
}

// replaceInWorkspace removes sp's row and makes another space with its ID in another workspace.
func (w *world) replaceInWorkspace(sp access.Space) access.Space {
	w.e.t.Helper()
	res, err := w.e.db.Exec(`INSERT INTO workspaces (name) VALUES ('Elsewhere')`)
	if err != nil {
		w.e.t.Fatal(err)
	}
	other, _ := res.LastInsertId()
	w.exec(`DELETE FROM spaces WHERE id = ?`, sp.ID)
	w.exec(`INSERT INTO spaces (id, workspace_id, kind, name) VALUES (?, ?, 'shared', 'Elsewhere')`, sp.ID, other)
	return w.generationOf(access.Space{ID: sp.ID, WorkspaceID: other, Kind: sp.Kind, Access: access.Write})
}

// The cache's entry for a space ID is of one workspace at a time: when a space in another workspace
// takes the ID, a change of that workspace's model (which drops its vectors) must reach the entry,
// and a change of the first workspace's must not.
func TestTheVectorCacheEntryFollowsTheWorkspaceOfTheSpaceThatTookTheID(t *testing.T) {
	w := newWorld(t)
	account := w.setModel()
	key := Embeddings{Account: account, Model: fakeModel}.key()
	old := w.generationOf(w.team)
	w.fillVectors(old, key, 8, 2, fakeDims, 1)
	qv := randomVector(rand.New(rand.NewPCG(3, 3)), fakeDims)
	cache := w.e.idx.vectors
	if found, _, err := cache.nearest(ctx, w.e.db, w.ws, []spaceRef{{old.ID, old.Generation}}, w.rank(key), qv); err != nil || len(found) == 0 {
		t.Fatalf("fixture: the old space's vectors: %d, %v", len(found), err)
	}

	fresh := w.replaceInWorkspace(old)
	ref := []spaceRef{{fresh.ID, fresh.Generation}}
	// The other workspace has a model of its own, and the replacement consents to it.
	var account2 int64
	if err := w.e.db.QueryRow(`INSERT INTO provider_accounts (workspace_id, name, display_name, kind, base_url, models)
		SELECT ?, 'embed2', 'Embed 2', kind, base_url, models FROM provider_accounts WHERE id = ? RETURNING id`, fresh.WorkspaceID, account).Scan(&account2); err != nil {
		t.Fatal(err)
	}
	w.exec(`INSERT INTO search_settings (workspace_id, account_id, model) VALUES (?, ?, ?)`, fresh.WorkspaceID, account2, fakeModel)
	w.exec(`INSERT INTO search_embedding_consent (space_id, account_id, model, base_url)
		SELECT ?, id, ?, base_url FROM provider_accounts WHERE id = ?`, fresh.ID, fakeModel, account2)
	key = Embeddings{Account: account2, Model: fakeModel}.key()
	w.fillVectors(fresh, key, 8, 2, fakeDims, 2)
	if found, _, err := cache.nearest(ctx, w.e.db, fresh.WorkspaceID, ref, w.rank(key), qv); err != nil || len(found) == 0 {
		t.Fatalf("fixture: the replacement's vectors: %d, %v", len(found), err)
	}
	held := func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.spaces[fresh.ID].snap != nil
	}

	// The first workspace's model changes: nothing of the replacement's.
	cache.changedWorkspace(w.ws)
	if !held() {
		t.Fatal("a change of another workspace's model dropped the replacement's vectors")
	}
	// The replacement's workspace changes its model, which drops its vectors from SQLite.
	w.exec(`DELETE FROM search_chunks WHERE doc_id IN (SELECT id FROM search_docs WHERE space_id = ?)`, fresh.ID)
	w.exec(`UPDATE search_docs SET embed_version = NULL, embed_key = NULL WHERE space_id = ?`, fresh.ID)
	cache.changedWorkspace(fresh.WorkspaceID)
	found, rest, err := cache.nearest(ctx, w.e.db, fresh.WorkspaceID, ref, w.rank(key), qv)
	if err != nil || len(rest) != 0 || len(found) != 0 {
		t.Fatalf("after the replacement's workspace changed its model the cache returned %d vectors (SQLite has none): %v, %v", len(found), rest, err)
	}
}

// The endpoint that embedded a query is the one whose vectors it is compared with: if the URL
// changes while the search waits for the embedding, and the consent and the vectors have caught up
// with the new URL, the old endpoint's query vector isn't ranked against them.
func TestASearchRanksOnlyVectorsOfTheEndpointThatEmbeddedItsQuery(t *testing.T) {
	w := newWorld(t)
	account := w.setModel()
	key := Embeddings{Account: account, Model: fakeModel}.key()
	team := w.generationOf(w.team)
	team.Access = access.Read
	w.put(w.owner, team, "fruit.md", "a kumquat in the team space")
	if _, err := w.e.idx.Sync(ctx, team); err != nil {
		t.Fatal(err)
	}
	w.fillVectors(team, key, 8, 2, fakeDims, 1)
	w.e.idx.afterQueryEmbedded = func() { w.e.idx.afterQueryEmbedded = nil; w.moveEndpoint(account, team.ID) }

	res := must[Results](t)(w.e.idx.searchIndex(ctx, w.ws, []access.Space{team}, Query{Text: "kumquat"}))
	if res.Mode != ModeHybrid {
		t.Fatalf("fixture: the search was %s", res.Mode)
	}
	for _, r := range res.Results {
		if r.Match != "text" {
			t.Fatalf("the query vector of the old endpoint ranked %q, a result by meaning, from the new endpoint's vectors", r.Path)
		}
	}
}

// A late notification for a space that was removed doesn't take the place of the one for the space
// that took its ID.
func TestAStaleSyncNotificationDoesNotReplaceTheReplacementsInTheQueue(t *testing.T) {
	w := newWorld(t)
	x := w.e.idx
	x.dirtyMu.Lock()
	clear(x.dirty)
	x.dirtyMu.Unlock()
	current := access.Space{ID: 77, WorkspaceID: w.ws, Generation: 9, Access: access.Write}
	stale := access.Space{ID: 77, WorkspaceID: w.ws, Generation: 6, Access: access.Write}
	x.changed(current)
	x.changed(stale) // arrives late
	x.dirtyMu.Lock()
	defer x.dirtyMu.Unlock()
	if got, ok := x.dirty[dirtyKey{77, 9}]; !ok || got.Generation != 9 {
		t.Fatalf("the replacement's notification was replaced: %+v", x.dirty)
	}
}

// Each request of an embedding job is sent only while what let the first still stands: consent
// withdrawn after the first request stops the second.
func TestAnEmbeddingJobStopsBetweenRequestsWhenConsentIsWithdrawn(t *testing.T) {
	w := newWorld(t)
	w.setModel()
	team := w.generationOf(w.team)
	var body strings.Builder
	for i := range 40 { // 40 sections that don't fit one passage together: two requests of 32 and 8
		fmt.Fprintf(&body, "# Part %d\n\n%s\n\n", i, strings.Repeat("kumquat ", 175))
	}
	w.put(w.owner, team, "long.md", body.String())
	if _, err := w.e.idx.Sync(ctx, team); err != nil {
		t.Fatal(err)
	}
	w.e.idx.afterEmbedRequest = func() {
		w.e.idx.afterEmbedRequest = nil
		w.exec(`DELETE FROM search_embedding_consent WHERE space_id = ?`, team.ID)
	}
	before := w.e.embed.calls.Load() // setting the model made a request of its own
	if _, err := w.e.idx.embedPending(ctx); err != nil {
		t.Fatal(err)
	}
	if n := w.e.embed.calls.Load() - before; n != 1 {
		t.Fatalf("the job made %d requests, want the first only", n)
	}
}

// A failure is recorded against the endpoint it happened at: if the workspace's endpoint changed
// while the request failed, the wait it earned isn't put on the new one.
func TestAnEmbeddingFailureIsNotRecordedAgainstAnEndpointThatChanged(t *testing.T) {
	w := newWorld(t)
	account := w.setModel()
	team := w.generationOf(w.team)
	w.put(w.owner, team, "fruit.md", "a kumquat")
	if _, err := w.e.idx.Sync(ctx, team); err != nil {
		t.Fatal(err)
	}
	w.e.embed.fail.Store(true)
	w.e.idx.afterEmbedRequest = func() { w.e.idx.afterEmbedRequest = nil; w.moveEndpoint(account, team.ID) }
	if _, err := w.e.idx.embedPending(ctx); err != nil {
		t.Fatal(err)
	}
	var tries int
	var after sql.NullString
	if err := w.e.db.QueryRow(`SELECT embed_tries, embed_after FROM search_docs WHERE space_id = ?`, team.ID).Scan(&tries, &after); err != nil {
		t.Fatal(err)
	}
	if tries != 0 || after.Valid {
		t.Fatalf("the failure at the old endpoint was recorded for the new one: %d tries, wait until %v", tries, after)
	}
}

// A document search releases results of a space only while the caller can still read it: they may
// lose access while the search runs.
func TestASearchReleasesNothingOfASpaceTheCallerLostWhileItRan(t *testing.T) {
	w := newWorld(t)
	w.put(w.owner, w.team, "fruit.md", "a kumquat in the team space")
	w.e.idx.beforeRelease = func() {
		w.e.idx.beforeRelease = nil
		w.exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.team.ID, w.alice.id)
	}
	rec := w.search(w.alice, &w.team, "kumquat")
	expect(t, rec, http.StatusOK)
	if got := decode[Results](t, rec); len(got.Results) != 0 {
		t.Fatalf("results of a space the caller lost access to: %+v", got.Results)
	}
}

// A consent change uses the caller's role as it is when the change is made, not the one seen
// before the request's body was read.
func TestAConsentChangeUsesTheCallersRoleAtTheTimeOfTheChange(t *testing.T) {
	w := newWorld(t)
	w.setModel()
	notes := w.createShared("Notes")
	w.addMember(notes, w.alice, access.RoleEditor)
	w.e.idx.afterConsentBody = func() {
		w.e.idx.afterConsentBody = nil
		w.exec(`UPDATE space_members SET role = 'viewer' WHERE space_id = ? AND user_id = ?`, notes.ID, w.alice.id)
	}
	rec := w.alice.do(http.MethodPut, embeddingURL(notes), map[string]bool{"enabled": true})
	expect(t, rec, http.StatusForbidden)
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM search_embedding_consent WHERE space_id = ?`, notes.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("consent given by a caller who was demoted: %d rows, %v", n, err)
	}
}

// A consent change that is refused leaves the cache of the space that has the ID alone.
func TestARefusedConsentChangeLeavesTheReplacementsCacheAlone(t *testing.T) {
	w := newWorld(t)
	account := w.setModel()
	key := Embeddings{Account: account, Model: fakeModel}.key()
	old := w.generationOf(w.team)
	fresh := w.replace(old, true)
	w.fillVectors(fresh, key, 8, 2, fakeDims, 1)
	qv := randomVector(rand.New(rand.NewPCG(4, 4)), fakeDims)
	cache := w.e.idx.vectors
	if found, _, err := cache.nearest(ctx, w.e.db, w.ws, []spaceRef{{fresh.ID, fresh.Generation}}, w.rank(key), qv); err != nil || len(found) == 0 {
		t.Fatalf("fixture: %d, %v", len(found), err)
	}
	if err := w.e.idx.setConsent(ctx, old, w.owner.id, false, nil, nil); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("a consent change for the old space: %v", err)
	}
	cache.mu.Lock()
	held := cache.spaces[fresh.ID].snap != nil
	cache.mu.Unlock()
	if !held {
		t.Fatal("a refused consent change evicted the replacement's vectors")
	}
}

// What an old space's load finds (it didn't fit the budget) is not put on the entry of the space
// that took the ID while it loaded.
func TestAnOldLoadThatDidNotFitDoesNotMarkTheReplacementsEntry(t *testing.T) {
	w := newWorld(t)
	account := w.setModel()
	key := Embeddings{Account: account, Model: fakeModel}.key()
	old := w.generationOf(w.team)
	w.fillVectors(old, key, 400, 2, fakeDims, 1) // more than the budget below holds
	qv := randomVector(rand.New(rand.NewPCG(6, 6)), fakeDims)
	cache := newVectorCache(40 << 10)

	reached, release := make(chan struct{}), make(chan struct{})
	cache.beforeInstall = func(sp spaceRef) {
		if sp.generation == old.Generation {
			close(reached)
			<-release
		}
	}
	oldDone := make(chan error, 1)
	go func() {
		_, _, err := cache.nearest(ctx, w.e.db, w.ws, []spaceRef{{old.ID, old.Generation}}, w.rank(key), qv)
		oldDone <- err
	}()
	<-reached // the old space's vectors are loaded (over the budget) and not yet recorded

	fresh := w.replace(old, true)
	w.fillVectors(fresh, key, 4, 2, fakeDims, 2)
	freshDone := make(chan []near, 1)
	go func() {
		found, rest, err := cache.nearest(ctx, w.e.db, w.ws, []spaceRef{{fresh.ID, fresh.Generation}}, w.rank(key), qv)
		if err != nil || len(rest) != 0 {
			t.Errorf("the replacement's search: %v, left to SQLite: %v", err, rest)
		}
		freshDone <- found
	}()
	spin(t, "the replacement's search to claim the entry", func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.spaces[fresh.ID].generation == fresh.Generation
	})
	close(release)
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	if found := <-freshDone; len(found) == 0 {
		t.Fatal("the replacement's search found nothing: it was sent to SQLite by the old space's over-budget load")
	}
}

// spin yields until cond holds, and fails if it doesn't in ten seconds.
func spin(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		default:
			runtime.Gosched()
		}
	}
}

// A document search that loses a space's readability while it runs releases nothing that says more
// of it either: no next page and no count of its documents waiting for embeddings.
func TestASearchThatLostASpaceReleasesNoNextPageAndNoPendingCount(t *testing.T) {
	w := newWorld(t)
	w.setModel() // team is consented, so its unembedded documents are counted
	w.put(w.owner, w.team, "a.md", "a kumquat")
	w.put(w.owner, w.team, "b.md", "another kumquat")
	w.e.idx.beforeRelease = func() {
		w.e.idx.beforeRelease = nil
		w.exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, w.team.ID, w.alice.id)
	}
	rec := w.alice.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces/%d/search?q=kumquat&limit=1", w.ws, w.team.ID), nil)
	expect(t, rec, http.StatusOK)
	got := decode[Results](t, rec)
	if len(got.Results) != 0 || got.NextOffset != nil || len(got.Notices) != 0 || strings.Contains(got.Notice, "embedded") {
		t.Fatalf("what a search that lost the space released: results %d, next offset %v, notices %+v, notice %q",
			len(got.Results), got.NextOffset, got.Notices, got.Notice)
	}
}

// The vector cache's snapshot is of one endpoint: its URL as well as the model. Consent for a new
// URL that is committed (its vectors dropped) before the cache is told must not leave a search of
// the new endpoint ranking the old endpoint's cached vectors.
func TestAWarmCacheOfOneURLIsNotRankedForAnother(t *testing.T) {
	w := newWorld(t)
	account := w.setModel()
	key := Embeddings{Account: account, Model: fakeModel}.key()
	team := w.generationOf(w.team)
	w.fillVectors(team, key, 8, 2, fakeDims, 1)
	qv := randomVector(rand.New(rand.NewPCG(8, 8)), fakeDims)
	in := spacesJSON(t, team)
	urlA := w.rank(key)
	for range 2 { // warm
		if found := must[[]near](t)(w.e.idx.nearest(ctx, w.ws, in, urlA, qv, 10)); len(found) == 0 {
			t.Fatal("fixture: nothing cached for the first endpoint")
		}
	}

	// Consent for the new URL is committed, which drops the old endpoint's vectors in SQLite; the
	// cache has not heard of it yet.
	w.moveEndpoint(account, team.ID)
	w.exec(`DELETE FROM search_chunks`)
	w.exec(`UPDATE search_docs SET embed_version = NULL, embed_key = NULL`)
	urlB := rankEndpoint{key: key, baseURL: "https://moved.example.test/v1"}
	got := must[[]near](t)(w.e.idx.nearest(ctx, w.ws, in, urlB, qv, 10))
	if want := w.e.idx.bruteNearest(t, in, urlB, qv, 10); len(want) != 0 || len(got) != 0 {
		t.Fatalf("the new endpoint's search ranked %d cached vectors of the old endpoint (SQLite has %d)", len(got), len(want))
	}
}

// What a cache load reads is the vectors of the endpoint the search holds, not whatever the space
// has under the model's key by the time the load runs.
func TestACacheLoadReadsOnlyTheVectorsOfTheEndpointTheSearchHolds(t *testing.T) {
	w := newWorld(t)
	account := w.setModel()
	key := Embeddings{Account: account, Model: fakeModel}.key()
	team := w.generationOf(w.team)
	w.fillVectors(team, key, 8, 2, fakeDims, 1)
	qv := randomVector(rand.New(rand.NewPCG(9, 9)), fakeDims)
	in := spacesJSON(t, team)
	urlA := w.rank(key)

	cache := w.e.idx.vectors
	cache.beforeRefresh = func(spaceRef) { // the search has passed its consent check and not loaded
		cache.beforeRefresh = nil
		w.moveEndpoint(account, team.ID)
		w.exec(`DELETE FROM search_chunks`)
		w.exec(`DELETE FROM search_docs WHERE space_id = ?`, team.ID)
		w.fillVectors(team, key, 4, 2, fakeDims, 2) // re-embedded at the new URL, under the same key
	}
	got := must[[]near](t)(w.e.idx.nearest(ctx, w.ws, in, urlA, qv, 10))
	if len(got) != 0 {
		t.Fatalf("a search of the first endpoint ranked %d vectors of the second", len(got))
	}
}
