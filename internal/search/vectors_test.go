package search

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/knowledge"
)

func (x *Index) bruteNearest(t *testing.T, in string, key rankEndpoint, qv []float32, limit int) []near {
	t.Helper()
	return rank(must[[]near](t)(x.nearestBrute(ctx, in, key, qv)), limit)
}

func TestVectorCacheMatchesBruteForce(t *testing.T) {
	w := newWorld(t)
	account := w.setModel()
	key := Embeddings{Account: account, Model: fakeModel}.key()
	mine := w.private[w.alice]
	w.consent(w.owner, w.secret)
	w.consent(w.alice, mine)
	const dims = 100
	w.fillVectors(w.team, key, 1200, 4, dims, 1)
	w.fillVectors(w.secret, key, 400, 7, dims, 2)
	w.fillVectors(mine, key, 200, 1, dims, 3)
	r := rand.New(rand.NewPCG(4, 4))
	w.fillVectors(w.team, key, 6, 3, 50, 5)
	same := randomVector(r, dims)
	var ties []int64
	for i, vs := range [][][]float32{{randomVector(r, dims), same, same}, {same}} {
		doc := w.fillVectors(w.team, key, len(vs), len(vs), dims, uint64(10+i))[0]
		ties = append(ties, doc)
		for seq, v := range vs {
			if _, err := w.e.idx.db.ExecContext(ctx, `UPDATE search_chunks SET vector = ? WHERE doc_id = ? AND seq = ?`,
				encodeVector(v), doc, seq); err != nil {
				t.Fatal(err)
			}
		}
	}

	all := []access.Space{w.team, w.secret, mine}
	queries := [][]float32{same}
	for range 2 {
		queries = append(queries, randomVector(r, dims))
	}
	check := func(t *testing.T, x *Index) {
		t.Helper()
		for _, spaces := range [][]access.Space{all, {w.team}, {w.secret, mine}} {
			in := spacesJSON(t, spaces...)
			for _, q := range queries {
				for _, limit := range []int{10, hybridDepth} {
					want := x.bruteNearest(t, in, w.rank(key), q, limit)
					for range 2 {
						got := must[[]near](t)(x.nearest(ctx, w.ws, in, w.rank(key), q, limit))
						if len(want) == 0 || !slices.Equal(got, want) {
							t.Fatalf("spaces %s, limit %d: %d found, want %d equal to brute force", in, limit, len(got), len(want))
						}
					}
				}
			}
		}
		got := must[[]near](t)(x.nearest(ctx, w.ws, spacesJSON(t, w.team), w.rank(key), same, 2))
		if got[0].similarity != got[1].similarity || got[0].doc != ties[0] || got[0].seq != 1 || got[1].doc != ties[1] {
			t.Fatalf("ties: %+v", got)
		}
	}

	x := w.e.idx
	t.Run("cached", func(t *testing.T) {
		check(t, x)
		if n := len(x.vectors.spaces); n != 3 || x.vectors.used == 0 {
			t.Fatalf("%d spaces cached, %d bytes", n, x.vectors.used)
		}
	})
	t.Run("off", func(t *testing.T) {
		x.vectors = nil
		check(t, x)
	})
	t.Run("partly", func(t *testing.T) {
		x.vectors = newVectorCache(1 << 40)
		must[[]near](t)(x.nearest(ctx, w.ws, spacesJSON(t, w.team), w.rank(key), same, 1))
		x.vectors = newVectorCache(x.vectors.used + 1)
		must[[]near](t)(x.nearest(ctx, w.ws, spacesJSON(t, all...), w.rank(key), same, 1))
		cached := map[bool]int{}
		for _, s := range x.vectors.spaces {
			cached[s.snap != nil]++
		}
		if cached[true] == 0 || cached[false] == 0 {
			t.Fatalf("cached: %v", cached)
		}
		check(t, x)
	})
}

func TestVectorCacheBudget(t *testing.T) {
	w := newWorld(t)
	account := w.setModel()
	key := Embeddings{Account: account, Model: fakeModel}.key()
	w.consent(w.owner, w.secret)
	w.fillVectors(w.team, key, 200, 2, 64, 1)
	w.fillVectors(w.secret, key, 200, 2, 64, 2)
	x := w.e.idx
	q := randomVector(rand.New(rand.NewPCG(3, 3)), 64)
	search := func(spaces ...access.Space) {
		t.Helper()
		in := spacesJSON(t, spaces...)
		got := must[[]near](t)(x.nearest(ctx, w.ws, in, w.rank(key), q, hybridDepth))
		if want := x.bruteNearest(t, in, w.rank(key), q, hybridDepth); !slices.Equal(got, want) {
			t.Fatalf("spaces %s: not as brute force", in)
		}
	}
	cached := func(sp access.Space) bool {
		s := x.vectors.spaces[sp.ID]
		return s != nil && s.snap != nil
	}
	const size = 200*(4*64+chunkOverhead) + 100*docOverhead

	x.vectors = newVectorCache(size + size/2)
	search(w.team)
	search(w.secret)
	if cached(w.team) || !cached(w.secret) || x.vectors.used != size {
		t.Fatalf("one space: team %v, secret %v, %d bytes", cached(w.team), cached(w.secret), x.vectors.used)
	}
	search(w.team, w.secret)
	search(w.team, w.secret)
	if cached(w.team) || !cached(w.secret) || x.vectors.used != size {
		t.Fatalf("both: team %v, secret %v, %d bytes", cached(w.team), cached(w.secret), x.vectors.used)
	}
	x.vectors = newVectorCache(size / 2)
	search(w.team)
	if cached(w.team) || x.vectors.used != 0 || x.vectors.spaces[w.team.ID].tooBig <= size/2 {
		t.Fatalf("too big: %+v", x.vectors.spaces[w.team.ID])
	}
	x.vectors.spaces[w.team.ID].tooBig = size / 2
	search(w.team)
	if x.vectors.spaces[w.team.ID].tooBig != size/2 {
		t.Fatal("loaded again without more room")
	}
	if _, err := w.e.idx.db.Exec(`DELETE FROM search_chunks WHERE doc_id IN
		(SELECT id FROM search_docs WHERE space_id = ? ORDER BY id LIMIT 90)`, w.team.ID); err != nil {
		t.Fatal(err)
	}
	search(w.team)
	if cached(w.team) {
		t.Fatal("tried again before tooBigRetry")
	}
	later := time.Now().Add(tooBigRetry)
	x.vectors.now = func() time.Time { return later }
	search(w.team)
	if !cached(w.team) || x.vectors.spaces[w.team.ID].tooBig != 0 {
		t.Fatalf("shrunk, after tooBigRetry: cached %v, %+v", cached(w.team), x.vectors.spaces[w.team.ID])
	}
}

func TestVectorCacheConcurrent(t *testing.T) {
	w := newWorld(t)
	account := w.setModel()
	key := Embeddings{Account: account, Model: fakeModel}.key()
	mine := w.private[w.owner]
	w.consent(w.owner, w.secret)
	w.consent(w.owner, mine)
	docs := w.fillVectors(w.team, key, 300, 3, 32, 1)
	w.fillVectors(w.secret, key, 200, 2, 32, 2)
	w.fillVectors(mine, key, 100, 1, 32, 3)
	x := w.e.idx
	x.vectors = newVectorCache(250 * (4*32 + chunkOverhead + docOverhead))
	sets := [][]access.Space{{w.team}, {w.secret}, {mine}, {w.team, mine}, {w.secret, mine}, {w.team, w.secret, mine}}
	q := randomVector(rand.New(rand.NewPCG(5, 5)), 32)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := range 6 {
		wg.Go(func() {
			for i := range 30 {
				if _, err := x.nearest(ctx, w.ws, spacesJSON(t, sets[(g+i)%len(sets)]...), w.rank(key), q, 50); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Go(func() {
		for i := range 60 {
			if i%10 == 0 {
				x.vectors.changedWorkspace(w.ws)
			}
			x.vectors.changedSpace(sets[i%3][0].ID)
			x.vectors.changedDocs(w.team.ID, docs[i%len(docs)])
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	var sum int64
	for _, s := range x.vectors.spaces {
		if s.snap != nil {
			sum += s.snap.bytes
		}
	}
	if sum != x.vectors.used || sum > x.vectors.budget {
		t.Fatalf("used %d, spaces hold %d, budget %d", x.vectors.used, sum, x.vectors.budget)
	}
	for _, set := range sets {
		in := spacesJSON(t, set...)
		if got, want := must[[]near](t)(x.nearest(ctx, w.ws, in, w.rank(key), q, 50)), x.bruteNearest(t, in, w.rank(key), q, 50); !slices.Equal(got, want) {
			t.Fatalf("spaces %s: not as brute force", in)
		}
	}
}

func (w *world) meaning(u *user, q string) []string {
	w.e.t.Helper()
	rec := w.search(u, nil, q)
	expect(w.e.t, rec, http.StatusOK)
	res := decode[Results](w.e.t, rec)
	if res.Mode != ModeHybrid {
		w.e.t.Fatalf("%s searching %q: %+v", u.name, q, res)
	}
	out := []string{}
	for _, r := range res.Results {
		if r.Match != "text" {
			out = append(out, fmt.Sprintf("%d/%s", r.SpaceID, r.Path))
		}
	}
	slices.Sort(out)
	return out
}

func docPath(sp access.Space, path string) string { return fmt.Sprintf("%d/%s", sp.ID, path) }

func TestVectorCacheFollowsChanges(t *testing.T) {
	for _, cache := range []bool{true, false} {
		t.Run(map[bool]string{true: "cached", false: "off"}[cache], func(t *testing.T) {
			testVectorsFollowChanges(t, cache)
		})
	}
}

func testVectorsFollowChanges(t *testing.T, cache bool) {
	w := newWorld(t)
	if !cache {
		w.e.idx.vectors = nil
	}
	account := w.setModel()
	mine := w.private[w.alice]
	w.consent(w.alice, mine)
	want := func(u *user, q string, paths ...string) {
		t.Helper()
		if paths == nil {
			paths = []string{}
		}
		slices.Sort(paths)
		if got := w.meaning(u, q); !slices.Equal(got, paths) {
			t.Fatalf("%s searching %q found by meaning %q, want %q", u.name, q, got, paths)
		}
	}
	bobs := w.private[w.bob]
	embed := func() {
		t.Helper()
		for _, sp := range []access.Space{w.team, mine, bobs} {
			must[string](t)(w.e.idx.Sync(ctx, sp))
		}
		w.e.embedAll()
	}
	fleet, garage := docPath(w.team, "fleet.md"), docPath(mine, "garage.md")
	w.put(w.owner, w.team, "fleet.md", "# Fleet\n\nOur automobile maintenance schedule.\n")
	w.put(w.alice, mine, "garage.md", "# Garage\n\nMy sedan.\n")
	embed()
	want(w.alice, "vehicle", fleet, garage)

	harbour := docPath(w.team, "harbour.md")
	w.put(w.owner, w.team, "harbour.md", "# Harbour\n\nThe maritime log.\n")
	want(w.alice, "vehicle", fleet, garage)
	embed()
	want(w.alice, "vehicle", fleet, garage, harbour)

	doc := must[knowledge.Doc](t)(w.e.kb.Read(ctx, w.team, "fleet.md", ""))
	must[knowledge.Result](t)(w.e.kb.Put(ctx, w.team, "fleet.md", []byte("# Fleet\n\nA ship on the ocean.\n"), doc.Version, by, ""))
	want(w.alice, "vehicle", garage, harbour)
	embed()
	want(w.alice, "vehicle", fleet, garage, harbour)

	expect(t, w.alice.do(http.MethodPut, embeddingURL(mine), map[string]bool{"enabled": false}), http.StatusOK)
	want(w.alice, "vehicle", fleet, harbour)
	w.consent(w.alice, mine)
	want(w.alice, "vehicle", fleet, harbour)
	embed()
	want(w.alice, "vehicle", fleet, garage, harbour)

	settings := fmt.Sprintf("/api/workspaces/%d/search/settings", w.ws)
	expect(t, w.owner.do(http.MethodPut, settings, map[string]any{"embeddings": nil}), http.StatusOK)
	expect(t, w.owner.do(http.MethodPut, settings,
		map[string]any{"embeddings": map[string]any{"account": account, "model": fakeModel}}), http.StatusOK)
	want(w.alice, "vehicle")
	embed()
	want(w.alice, "vehicle", fleet, garage, harbour)

	other := newFakeEmbeddings(t)
	expect(t, w.owner.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/providers/%d", w.ws, account),
		map[string]string{"base_url": other.srv.URL + "/v1"}), http.StatusOK)
	w.consent(w.owner, w.team)
	want(w.alice, "vehicle")
	if w.e.spaceChunks(mine) == 0 {
		t.Fatal("the vectors of a space consented to the old endpoint were dropped")
	}
	embed()
	want(w.alice, "vehicle", fleet, harbour)
	expect(t, w.owner.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/providers/%d", w.ws, account),
		map[string]string{"base_url": w.e.embed.srv.URL + "/v1"}), http.StatusOK)
	want(w.alice, "vehicle", garage)

	w.consent(w.bob, bobs)
	w.consent(w.owner, w.team)
	w.put(w.bob, bobs, "bike.md", "# Bike\n\nMy own vehicle.\n")
	embed()
	want(w.bob, "vehicle", fleet, harbour, docPath(bobs, "bike.md"))
	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", w.ws, w.team.ID, w.bob.id), nil),
		http.StatusNoContent)
	want(w.bob, "vehicle", docPath(bobs, "bike.md"))
	key := Embeddings{Account: account, Model: fakeModel}.key()
	qv := normalize(fakeVector("vehicle"))
	if found := must[[]near](t)(w.e.idx.nearest(ctx, w.ws, spacesJSON(t, bobs), w.rank(key), qv, 10)); len(found) != 1 {
		t.Fatalf("bob's spaces: %+v", found)
	}

	in := spacesJSON(t, w.team, mine)
	if n := len(must[[]near](t)(w.e.idx.nearest(ctx, w.ws, in, w.rank(key), qv, 10))); n != 3 {
		t.Fatalf("%d documents found before the space was deleted, want 3", n)
	}
	if _, err := w.e.idx.db.ExecContext(ctx, `DELETE FROM spaces WHERE id = ?`, w.team.ID); err != nil {
		t.Fatal(err)
	}
	found := must[[]near](t)(w.e.idx.nearest(ctx, w.ws, in, w.rank(key), qv, 10))
	if len(found) != 1 {
		t.Fatalf("after the team space was deleted: %+v", found)
	}
	if n := w.e.idx.bruteNearest(t, in, w.rank(key), qv, 10); !slices.Equal(found, n) {
		t.Fatalf("after the team space was deleted: %+v, brute force %+v", found, n)
	}
}
