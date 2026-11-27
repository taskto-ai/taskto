package search

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"testing"

	"taskto.ai/taskto/internal/access"
)

const benchDims = 768

func randomVector(r *rand.Rand, dims int) []float32 {
	v := make([]float64, dims)
	for i := range v {
		v[i] = r.NormFloat64()
	}
	return normalize(v)
}

func (w *world) fillVectors(sp access.Space, key string, n, perDoc, dims int, seed uint64) []int64 {
	t := w.e.t
	t.Helper()
	r := rand.New(rand.NewPCG(seed, 1))
	tx, err := w.e.idx.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	docStmt, err := tx.PrepareContext(ctx, `INSERT INTO search_docs (space_id, path, version, markdown, title, embed_version, embed_key)
		VALUES (?, ?, ?, 1, ?, ?, ?) RETURNING id`)
	if err != nil {
		t.Fatal(err)
	}
	chunkStmt, err := tx.PrepareContext(ctx, `INSERT INTO search_chunks (doc_id, seq, text, vector) VALUES (?, ?, '', ?)`)
	if err != nil {
		t.Fatal(err)
	}
	var docs []int64
	for i := 0; i < n; i += perDoc {
		version := fmt.Sprintf("v%d-%d", seed, i)
		var id int64
		if err := docStmt.QueryRowContext(ctx, sp.ID, fmt.Sprintf("bench/%d-%d.md", seed, i), version, "bench", version, key).
			Scan(&id); err != nil {
			t.Fatal(err)
		}
		docs = append(docs, id)
		for seq := range min(perDoc, n-i) {
			if _, err := chunkStmt.ExecContext(ctx, id, seq, encodeVector(randomVector(r, dims))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return docs
}

func spacesJSON(t testing.TB, spaces ...access.Space) string {
	entries := make([]scopeEntry, len(spaces))
	for i, sp := range spaces {
		entries[i] = scopeEntry{ID: sp.ID, Workspace: sp.WorkspaceID, Generation: sp.Generation}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func BenchmarkNearest(b *testing.B) {
	for _, n := range []int{20_000, 100_000, 200_000} {
		w := newWorld(b)
		account := w.setModel()
		key := Embeddings{Account: account, Model: fakeModel}.key()
		w.fillVectors(w.team, key, n, 4, benchDims, 1)
		in := spacesJSON(b, w.team)
		q := randomVector(rand.New(rand.NewPCG(2, 2)), benchDims)
		idx := w.e.idx
		name := fmt.Sprintf("%dk", n/1000)
		b.Run("brute/"+name, func(b *testing.B) {
			for b.Loop() {
				found, err := idx.nearestBrute(ctx, in, w.rank(key), q)
				if err != nil {
					b.Fatal(err)
				}
				rank(found, hybridDepth)
			}
		})
		b.Run("cached/"+name, func(b *testing.B) {
			if _, err := idx.nearest(ctx, w.ws, in, w.rank(key), q, hybridDepth); err != nil {
				b.Fatal(err)
			}
			for b.Loop() {
				if _, err := idx.nearest(ctx, w.ws, in, w.rank(key), q, hybridDepth); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("reload/"+name, func(b *testing.B) {
			for b.Loop() {
				idx.vectors.changedSpace(w.team.ID)
				if _, err := idx.nearest(ctx, w.ws, in, w.rank(key), q, hybridDepth); err != nil {
					b.Fatal(err)
				}
			}
		})
		if used := idx.vectors.used; used > 0 {
			b.Logf("%s: the cache holds %d MiB", name, used>>20)
		}
	}
}
