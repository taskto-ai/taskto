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

// The vector cache keeps the vectors semantic search compares in memory, decoded, so that a
// search doesn't read every one of them from SQLite: about 15 times faster on one core,
// and the comparisons are spread over all of them.
//
// It is sound because one server owns the data directory (internal/lockfile), and every
// write to the vectors is made by this package, which tells the cache what it touched
// once the write is committed: a document embedded or changed, a space whose consent
// changed, a workspace whose model changed. The next search of the space reloads that,
// before it compares anything, so a change is found at once.
//
// What isn't cached is what decides which vectors may be compared: every search asks
// SQLite which of the spaces it may look in are consented to the workspace's endpoint as
// it is now (embeddableSQL), so an account's base URL changed, or a space or an account
// deleted, takes effect at once without the cache hearing of it. The cache is per space,
// and a search only takes the spaces it was given: access is the caller's, as without it.
//
// Memory is bounded by a budget, the least recently searched spaces making room for
// others; a space that doesn't fit is read from SQLite, as without the cache, which stays
// the fallback and, in tests, the reference: both compare with dot32, so their scores are
// equal to the bit.

// DefaultVectorCache is the vector cache's default budget, in bytes: about 340,000
// passages of 768 dimensions.
const DefaultVectorCache = 1 << 30

const (
	// chunkOverhead and docOverhead are what the cache holds beside the vectors, about, for
	// each passage and each document.
	chunkOverhead = 32
	docOverhead   = 64
	// scanPart is how many documents a goroutine of a scan takes at a time.
	scanPart = 256
)

// vectorCache is the vector cache. Its methods do nothing on a nil one: the cache is off.
type vectorCache struct {
	budget int64

	mu     sync.Mutex
	spaces map[int64]*cachedSpace
	used   int64  // bytes of every space's snap
	clock  uint64 // counts searches, for least recently used
	now    func() time.Time
}

// tooBigRetry is how long a space that didn't fit is read from SQLite before a load is tried
// again with no more room than before: it may have shrunk (documents deleted) meanwhile, and
// waiting keeps a big space's every search from paying for a load that fails.
const tooBigRetry = 10 * time.Minute

// cachedSpace is a space's entry in the cache.
type cachedSpace struct {
	id, workspace int64
	// refresh is held while the space's vectors are brought up to date: one at a time.
	refresh sync.Mutex
	// snap is the space's vectors, nil if not loaded; set with both refresh and the
	// cache's mu held, so it can be read with either.
	snap *spaceVectors

	// Under the cache's mu: what changed since snap was read, the whole space (reset) or
	// some documents; when it was last searched; and, if it didn't fit in the budget, the
	// bytes it had reached.
	reset    bool
	stale    map[int64]bool
	lastUse  uint64
	tooBig   int64
	tooBigAt time.Time // when it last didn't fit
}

// spaceVectors are a space's vectors of one embeddings model, as SQLite had them. They
// never change once made: a refresh makes new ones, so a search can read them unlocked.
type spaceVectors struct {
	key   string
	docs  []*docVectors
	bytes int64
}

// docVectors are a document's passages' vectors, in the order of their seq.
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

// changedDocs tells the cache that the vectors of space's docs may have changed: a write
// committed. The next search reloads them.
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

// changedSpace tells the cache that all of space's vectors may have changed, or may not be
// compared any more: its consent changed. Its memory is freed now if no search is using it.
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

// changedWorkspace is changedSpace for every space of a workspace: its model changed.
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

// resetLocked marks s to be loaded again, and drops what it has if it can now.
func (c *vectorCache) resetLocked(s *cachedSpace) {
	s.reset, s.stale, s.tooBig = true, nil, 0
	if s.refresh.TryLock() {
		c.dropLocked(s)
		s.refresh.Unlock()
	}
}

// dropLocked drops s's vectors; s.refresh and c.mu are held.
func (c *vectorCache) dropLocked(s *cachedSpace) {
	if s.snap != nil {
		c.used -= s.snap.bytes
		s.snap = nil
	}
}

// nearest compares qv with the vectors of model key of the cached spaces among spaces (of
// workspace, and which the caller may read and are consented to the endpoint), and returns
// each document's passage most like it, unordered, and the spaces that aren't cached.
func (c *vectorCache) nearest(ctx context.Context, db *sql.DB, workspace int64, spaces []int64, key string,
	qv []float32) (found []near, rest []int64, err error) {
	if c == nil {
		return nil, spaces, nil
	}
	keep := make(map[int64]bool, len(spaces))
	for _, id := range spaces {
		keep[id] = true
	}
	var snaps []*spaceVectors
	for _, id := range spaces {
		snap, err := c.snapshot(ctx, db, id, workspace, key, keep)
		if err != nil {
			return nil, nil, err
		}
		if snap == nil {
			rest = append(rest, id)
			continue
		}
		snaps = append(snaps, snap)
	}
	return scan(snaps, qv), rest, nil
}

// snapshot returns space id's vectors of key, brought up to date, or nil if they don't
// fit in the budget beside those of the spaces to keep (the search's).
func (c *vectorCache) snapshot(ctx context.Context, db *sql.DB, id, workspace int64, key string, keep map[int64]bool) (*spaceVectors, error) {
	c.mu.Lock()
	s := c.spaces[id]
	if s == nil {
		s = &cachedSpace{id: id, workspace: workspace}
		c.spaces[id] = s
	}
	c.clock++
	s.lastUse = c.clock
	c.mu.Unlock()

	s.refresh.Lock()
	defer s.refresh.Unlock()
	c.mu.Lock()
	cur := s.snap
	full := s.reset || cur == nil || cur.key != key
	docs := s.stale
	limit := c.budget
	for _, o := range c.spaces {
		if o != s && keep[o.id] && o.snap != nil {
			limit -= o.snap.bytes
		}
	}
	if full && s.tooBig > 0 && limit <= s.tooBig && c.now().Sub(s.tooBigAt) < tooBigRetry {
		// It didn't fit, and there is no more room now: SQLite, without trying again. What
		// changed meanwhile doesn't matter: it is loaded whole when it fits.
		c.dropLocked(s)
		s.stale = nil
		c.mu.Unlock()
		return nil, nil
	}
	s.reset, s.stale = false, nil
	if full {
		c.dropLocked(s) // of no use now: its memory goes before the load
	}
	c.mu.Unlock()
	if !full && len(docs) == 0 {
		return cur, nil
	}

	var next *spaceVectors
	var err error
	if full {
		next, err = loadVectors(ctx, db, id, key, nil, limit)
	} else {
		var changed *spaceVectors
		if changed, err = loadVectors(ctx, db, id, key, docs, math.MaxInt64); err == nil {
			next = cur.with(changed, docs)
		}
	}
	var over errOverBudget
	if errors.As(err, &over) {
		c.mu.Lock()
		c.dropLocked(s)
		s.tooBig, s.tooBigAt = int64(over), c.now()
		c.mu.Unlock()
		return nil, nil
	}
	if err != nil {
		c.mu.Lock()
		s.reset = true // whatever it was asked to reload, it hasn't
		c.mu.Unlock()
		return nil, err
	}
	if !c.install(s, next, keep) {
		return nil, nil
	}
	return next, nil
}

// install makes next s's vectors, making room for them by dropping the least recently
// searched spaces other than those to keep; or drops s's if there isn't enough, and
// reports false. s.refresh is held.
func (c *vectorCache) install(s *cachedSpace, next *spaceVectors, keep map[int64]bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
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
			if o.refresh.TryLock() { // else a search is refreshing it: it stays
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

// with returns sv with the documents in docs replaced by those of changed (a document of
// docs not in changed has no vectors any more).
func (sv *spaceVectors) with(changed *spaceVectors, docs map[int64]bool) *spaceVectors {
	out := &spaceVectors{key: sv.key, docs: make([]*docVectors, 0, len(sv.docs)+len(changed.docs))}
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

// errOverBudget is for vectors that need more memory than allowed; it is the bytes they
// had reached.
type errOverBudget int64

func (e errOverBudget) Error() string { return "search: the vectors don't fit in the vector cache" }

// loadVectors reads the vectors of model key of space's documents (or of docs only, if not
// nil) that nearestBrute would compare, but for consent, which the caller checks. Past
// limit bytes, it stops and returns errOverBudget.
func loadVectors(ctx context.Context, db *sql.DB, space int64, key string, docs map[int64]bool, limit int64) (*spaceVectors, error) {
	query := `SELECT c.doc_id, c.seq, c.vector FROM search_chunks c JOIN search_docs d ON d.id = c.doc_id
		WHERE d.space_id = ? AND d.embed_key = ? AND d.embed_version = d.version`
	args := []any{space, key}
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
	// In the order nearestBrute reads them, which picks the first of a document's most
	// alike passages: not sorted, which would cost more than reading them.
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sv := &spaceVectors{key: key}
	// A document's vectors are read into one array, then copied into one of their size.
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

// scan returns each document of snaps with its passage most like qv, unordered, comparing
// on every core.
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

// nearest returns d's passage most like qv, as nearestBrute picks it: the first of the
// most alike, of qv's size. ok is false if none is of its size.
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

// decodeVectorInto decodes b, a vector encodeVector encoded, into v, of len(b)/4 floats.
func decodeVectorInto(v []float32, b []byte) {
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
}

// dot32 is the dot product of q and v, of the same length, summed in eight lanes, which a
// core works on at once: several times faster than one. Every comparison of vectors uses
// it, so that the cache and SQLite give the same scores, to the bit.
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
