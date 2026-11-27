package search

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const DefaultVectorCache = 1 << 30

const (
	chunkOverhead = 32
	docOverhead   = 64
	scanPart      = 256
)

type vectorCache struct {
	budget int64

	mu     sync.Mutex
	spaces map[int64]*cachedSpace
	used   int64
	clock  uint64
	now    func() time.Time

	beforeRefresh func(sp spaceRef)
	beforeInstall func(sp spaceRef)
}

const tooBigRetry = 10 * time.Minute

type cachedSpace struct {
	id, workspace int64
	generation    int64
	refresh       sync.Mutex
	snap          *spaceVectors

	reset    bool
	stale    map[int64]bool
	lastUse  uint64
	tooBig   int64
	tooBigAt time.Time
}

type spaceVectors struct {
	generation int64
	key        string
	baseURL    string
	docs       []*docVectors
	bytes      int64
}

type docVectors struct {
	id    int64
	seqs  []int
	vecs  [][]float32
	bytes int64
}

func newVectorCache(budget int64) *vectorCache {
	if budget < 0 {
		return nil
	}
	return &vectorCache{budget: cmp.Or(budget, DefaultVectorCache), spaces: map[int64]*cachedSpace{}, now: time.Now}
}

func (c *vectorCache) changedDocs(space int64, docs ...int64) {
	if c == nil || len(docs) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.spaces[space]
	if s == nil || s.reset {
		return
	}
	if s.stale == nil {
		s.stale = map[int64]bool{}
	}
	for _, d := range docs {
		s.stale[d] = true
	}
}

func (c *vectorCache) changedSpace(space int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.spaces[space]; s != nil {
		c.resetLocked(s)
	}
}

func (c *vectorCache) changedWorkspace(workspace int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.spaces {
		if s.workspace == workspace {
			c.resetLocked(s)
		}
	}
}

func (c *vectorCache) resetLocked(s *cachedSpace) {
	s.reset, s.stale, s.tooBig = true, nil, 0
	if s.refresh.TryLock() {
		c.dropLocked(s)
		s.refresh.Unlock()
	}
}

func (c *vectorCache) dropLocked(s *cachedSpace) {
	if s.snap != nil {
		c.used -= s.snap.bytes
		s.snap = nil
	}
}

type spaceRef struct{ id, generation int64 }

func (c *vectorCache) nearest(ctx context.Context, db *sql.DB, workspace int64, spaces []spaceRef, ep rankEndpoint,
	qv []float32) (found []near, rest []int64, err error) {
	if c == nil {
		ids := make([]int64, len(spaces))
		for i, sp := range spaces {
			ids[i] = sp.id
		}
		return nil, ids, nil
	}
	keep := make(map[int64]bool, len(spaces))
	for _, sp := range spaces {
		keep[sp.id] = true
	}
	var snaps []*spaceVectors
	for _, sp := range spaces {
		snap, err := c.snapshot(ctx, db, sp, workspace, ep, keep)
		if err != nil {
			return nil, nil, err
		}
		if snap == nil {
			rest = append(rest, sp.id)
			continue
		}
		snaps = append(snaps, snap)
	}
	return scan(snaps, qv), rest, nil
}

func (c *vectorCache) snapshot(ctx context.Context, db *sql.DB, sp spaceRef, workspace int64, ep rankEndpoint, keep map[int64]bool) (*spaceVectors, error) {
	id := sp.id
	c.mu.Lock()
	s := c.spaces[id]
	if s == nil {
		s = &cachedSpace{id: id, workspace: workspace, generation: sp.generation}
		c.spaces[id] = s
	}
	switch {
	case sp.generation < s.generation:
		c.mu.Unlock()
		return nil, nil
	case sp.generation > s.generation:
		c.resetLocked(s)
		s.generation, s.workspace = sp.generation, workspace
	}
	c.clock++
	s.lastUse = c.clock
	c.mu.Unlock()
	if c.beforeRefresh != nil {
		c.beforeRefresh(sp)
	}

	s.refresh.Lock()
	defer s.refresh.Unlock()
	c.mu.Lock()
	if s.generation != sp.generation {
		c.mu.Unlock()
		return nil, nil
	}
	cur := s.snap
	full := s.reset || cur == nil || cur.key != ep.key || cur.baseURL != ep.baseURL || cur.generation != sp.generation
	docs := s.stale
	limit := c.budget
	for _, o := range c.spaces {
		if o != s && keep[o.id] && o.snap != nil {
			limit -= o.snap.bytes
		}
	}
	if full && s.tooBig > 0 && limit <= s.tooBig && c.now().Sub(s.tooBigAt) < tooBigRetry {
		c.dropLocked(s)
		s.stale = nil
		c.mu.Unlock()
		return nil, nil
	}
	s.reset, s.stale = false, nil
	if full {
		c.dropLocked(s)
	}
	c.mu.Unlock()
	if !full && len(docs) == 0 {
		return cur, nil
	}

	var next *spaceVectors
	var err error
	if full {
		next, err = loadVectors(ctx, db, id, sp.generation, ep, nil, limit)
	} else {
		var changed *spaceVectors
		if changed, err = loadVectors(ctx, db, id, sp.generation, ep, docs, math.MaxInt64); err == nil {
			next = cur.with(changed, docs)
		}
	}
	if c.beforeInstall != nil {
		c.beforeInstall(sp)
	}
	var over errOverBudget
	if errors.As(err, &over) {
		c.mu.Lock()
		if s.generation == sp.generation {
			c.dropLocked(s)
			s.tooBig, s.tooBigAt = int64(over), c.now()
		}
		c.mu.Unlock()
		return nil, nil
	}
	if err != nil {
		c.mu.Lock()
		if s.generation == sp.generation {
			s.reset = true
		}
		c.mu.Unlock()
		return nil, err
	}
	if !c.install(s, next, keep) {
		return nil, nil
	}
	return next, nil
}

func (c *vectorCache) install(s *cachedSpace, next *spaceVectors, keep map[int64]bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.generation != next.generation {
		return false
	}
	c.dropLocked(s)
	if c.used+next.bytes > c.budget {
		var victims []*cachedSpace
		for _, o := range c.spaces {
			if o.snap != nil && !keep[o.id] {
				victims = append(victims, o)
			}
		}
		slices.SortFunc(victims, func(a, b *cachedSpace) int { return cmp.Compare(a.lastUse, b.lastUse) })
		for _, o := range victims {
			if c.used+next.bytes <= c.budget {
				break
			}
			if o.refresh.TryLock() {
				c.dropLocked(o)
				o.refresh.Unlock()
			}
		}
		if c.used+next.bytes > c.budget {
			s.tooBig, s.tooBigAt = next.bytes, c.now()
			return false
		}
	}
	s.snap = next
	s.tooBig = 0
	c.used += next.bytes
	return true
}

func (sv *spaceVectors) with(changed *spaceVectors, docs map[int64]bool) *spaceVectors {
	out := &spaceVectors{generation: sv.generation, key: sv.key, baseURL: sv.baseURL, docs: make([]*docVectors, 0, len(sv.docs)+len(changed.docs))}
	for _, d := range sv.docs {
		if !docs[d.id] {
			out.docs = append(out.docs, d)
			out.bytes += d.bytes
		}
	}
	out.docs = append(out.docs, changed.docs...)
	out.bytes += changed.bytes
	return out
}

type errOverBudget int64

func (e errOverBudget) Error() string { return "search: the vectors don't fit in the vector cache" }

func loadVectors(ctx context.Context, db *sql.DB, space, generation int64, ep rankEndpoint, docs map[int64]bool, limit int64) (*spaceVectors, error) {
	query := `SELECT c.doc_id, c.seq, c.vector FROM search_chunks c JOIN search_docs d ON d.id = c.doc_id
		JOIN spaces sp ON sp.id = d.space_id AND (? = 0 OR sp.generation = ?)
		JOIN search_settings e ON e.workspace_id = sp.workspace_id
		WHERE d.space_id = ? AND d.embed_key = ? AND d.embed_version = d.version
			AND ` + keySQL + ` = ? AND ` + embeddableAtSQL
	args := []any{generation, generation, space, ep.key, ep.key, ep.baseURL}
	if docs != nil {
		ids := make([]int64, 0, len(docs))
		for id := range docs {
			ids = append(ids, id)
		}
		raw, err := json.Marshal(ids)
		if err != nil {
			return nil, err
		}
		query += ` AND d.id IN (SELECT value FROM json_each(?))`
		args = append(args, string(raw))
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sv := &spaceVectors{generation: generation, key: ep.key, baseURL: ep.baseURL}
	type loading struct {
		*docVectors
		data []float32
		ends []int
	}
	var docsRead []*loading
	byID := map[int64]*loading{}
	var vec sql.RawBytes
	for rows.Next() {
		var doc int64
		var seq int
		if err := rows.Scan(&doc, &seq, &vec); err != nil {
			return nil, err
		}
		d := byID[doc]
		if d == nil {
			d = &loading{docVectors: &docVectors{id: doc, bytes: docOverhead}}
			byID[doc] = d
			docsRead = append(docsRead, d)
			sv.bytes += docOverhead
		}
		n := len(d.data)
		d.data = slices.Grow(d.data, len(vec)/4)[:n+len(vec)/4]
		decodeVectorInto(d.data[n:], vec)
		d.seqs = append(d.seqs, seq)
		d.ends = append(d.ends, len(d.data))
		size := int64(len(vec) + chunkOverhead)
		d.bytes += size
		sv.bytes += size
		if sv.bytes > limit {
			return nil, errOverBudget(sv.bytes)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sv.docs = make([]*docVectors, len(docsRead))
	for i, d := range docsRead {
		data := slices.Clone(d.data)
		d.vecs = make([][]float32, len(d.ends))
		start := 0
		for j, end := range d.ends {
			d.vecs[j] = data[start:end:end]
			start = end
		}
		sv.docs[i] = d.docVectors
	}
	return sv, nil
}

func scan(snaps []*spaceVectors, qv []float32) []near {
	var parts [][]*docVectors
	for _, sv := range snaps {
		for start := 0; start < len(sv.docs); start += scanPart {
			parts = append(parts, sv.docs[start:min(start+scanPart, len(sv.docs))])
		}
	}
	scanParts := func(next func() int) []near {
		var out []near
		for i := next(); i < len(parts); i = next() {
			for _, d := range parts[i] {
				if n, ok := d.nearest(qv); ok {
					out = append(out, n)
				}
			}
		}
		return out
	}
	workers := min(runtime.GOMAXPROCS(0), len(parts))
	if workers <= 1 {
		i := -1
		return scanParts(func() int { i++; return i })
	}
	var at atomic.Int64
	next := func() int { return int(at.Add(1) - 1) }
	found := make([][]near, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() { found[w] = scanParts(next) })
	}
	wg.Wait()
	return slices.Concat(found...)
}

func (d *docVectors) nearest(qv []float32) (n near, ok bool) {
	for i, v := range d.vecs {
		if len(v) != len(qv) {
			continue
		}
		if sim := float64(dot32(qv, v)); !ok || sim > n.similarity {
			n, ok = near{doc: d.id, seq: d.seqs[i], similarity: sim}, true
		}
	}
	return n, ok
}

func decodeVectorInto(v []float32, b []byte) {
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
}

func dot32(q, v []float32) float32 {
	v = v[:len(q)]
	var s0, s1, s2, s3, s4, s5, s6, s7 float32
	i := 0
	for ; i+8 <= len(q); i += 8 {
		a, b := q[i:i+8:i+8], v[i:i+8:i+8]
		s0 += a[0] * b[0]
		s1 += a[1] * b[1]
		s2 += a[2] * b[2]
		s3 += a[3] * b[3]
		s4 += a[4] * b[4]
		s5 += a[5] * b[5]
		s6 += a[6] * b[6]
		s7 += a[7] * b[7]
	}
	for ; i < len(q); i++ {
		s0 += q[i] * v[i]
	}
	return ((s0 + s1) + (s2 + s3)) + ((s4 + s5) + (s6 + s7))
}
