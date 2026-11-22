package search

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/egress"
)

// Limits of a search.
const (
	DefaultLimit = 20
	MaxLimit     = 100
	// MaxOffset is the deepest a search pages: past it, refine the query.
	MaxOffset = 1000
	// MaxQueryLen is the longest query, in bytes; maxTerms the most words and phrases of
	// one that count, and maxTokens the most words in all, counting each character of an
	// unspaced script (see segment) as one.
	MaxQueryLen = 500
	maxTerms    = 32
	maxTokens   = 64
)

// Query is a search.
type Query struct {
	Text   string
	Limit  int // 0 for DefaultLimit
	Offset int
}

// Segment is a piece of a snippet; Match marks the words that matched.
type Segment struct {
	Text  string `json:"text"`
	Match bool   `json:"match,omitempty"`
}

// Result is a document found.
type Result struct {
	SpaceID   int64  `json:"space_id"`
	SpaceName string `json:"space_name"`
	Path      string `json:"path"`
	Title     string `json:"title"`
	// Snippet is the passage of the text that matched, as plain text in pieces: never
	// markup, so it is safe to show as it is.
	Snippet []Segment `json:"snippet"`
	// Score orders the results, higher first. It compares results of one search only.
	Score float64 `json:"score"`
	// Match says what found it: "text", "semantic" or both.
	Match string `json:"match"`
	// Version is the document's version, and Commit main's commit, that the index had:
	// read the document at Commit to see what was found.
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// The modes of a search.
const (
	ModeFullText = "fulltext"
	ModeHybrid   = "hybrid"
)

// Results are a page of results.
type Results struct {
	Results []Result `json:"results"`
	// Mode is ModeHybrid when semantic search took part, ModeFullText when not; Notice
	// then says why, or what semantic search can't find yet.
	Mode   string `json:"mode"`
	Notice string `json:"notice,omitempty"`
	// Notices are what Notice says, as codes, for a client to say in its own language.
	Notices []Notice `json:"notices,omitempty"`
	// NextOffset is the next page's offset, if there are more results.
	NextOffset *int `json:"next_offset,omitempty"`
}

// Notice is one part of a search's notice: one of the Notice… codes, and for
// NoticePending, how many documents are still being embedded.
type Notice struct {
	Code  string `json:"code"`
	Count int    `json:"count,omitempty"`
}

// The codes of a search's notices. Each keeps its meaning once released.
const (
	NoticeOff       = "embeddings_off"
	NoticeFailed    = "embeddings_failed"
	NoticeRefused   = "egress_refused"
	NoticeNoConsent = "no_consent"
	NoticePending   = "pending"
	NoticeBehind    = "behind"
)

// note sets the notice: text, and its code.
func (r *Results) note(n Notice, text string) {
	r.Notice, r.Notices = text, []Notice{n}
}

// ErrInvalid is for a query that can't be run; its message says why.
var ErrInvalid = errors.New("invalid search")

// invalidError is ErrInvalid with the API's code for it and its message's params
// (internal/apierr); without one, it is apierr.CodeBadRequest.
type invalidError struct {
	error
	code   apierr.Code
	params apierr.Params
}

func (e *invalidError) Unwrap() error { return e.error }

func invalid(code apierr.Code, params apierr.Params, format string, args ...any) error {
	return &invalidError{error: fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...), code: code, params: params}
}

const (
	noticeOff       = "Semantic search is off: no embeddings model is set for this workspace. These are full-text results."
	noticeFailed    = "The embeddings model didn't answer, so these are full-text results only."
	noticeRefused   = "The server's egress policy doesn't allow the embeddings model's endpoint, so these are full-text results only."
	noticeBehind    = "The index is catching up with recent changes: some results may be missing or out of date."
	noticeNoConsent = "Semantic search is off for these spaces until one of their members turns it on for this workspace's embeddings model. These are full-text results."
	noticePending   = "%d documents are still being embedded; until they are, only their text is searched."
)

// queryTimeout bounds the call that embeds a query.
const queryTimeout = 15 * time.Second

// Search finds q in spaces, which must be of workspaceID and readable by the caller: the
// space RequireSpace checked, or the caller's Readable spaces. Each space is synced with
// git first. It ranks by full text alone without an embeddings model, and fuses full-text
// and semantic ranks with one.
func (x *Index) Search(ctx context.Context, workspaceID int64, spaces []access.Space, q Query) (Results, error) {
	match, err := checkQuery(&q)
	if err != nil {
		return Results{}, err
	}
	names := map[int64]string{}
	ids := []int64{}
	behind := false
	for _, sp := range spaces {
		// Never anything else, whatever the caller passes.
		if sp.WorkspaceID != workspaceID || !sp.Access.Allows(access.Read) {
			continue
		}
		synced, err := x.syncForSearch(ctx, sp)
		if ctx.Err() != nil {
			return Results{}, ctx.Err()
		}
		if err != nil {
			x.log.Error("search: syncing a space before a search; its results may be behind", "space", sp.ID, "err", err)
		}
		behind = behind || !synced
		names[sp.ID] = sp.Name
		ids = append(ids, sp.ID)
	}
	if len(ids) == 0 {
		// No space to search: nothing found, and nothing to say about semantic search.
		return Results{Results: []Result{}, Mode: ModeFullText}, nil
	}
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return Results{}, err
	}
	in := string(idsJSON)

	res := Results{Results: []Result{}, Mode: ModeFullText}
	var qv []float32
	e, ep, err := x.embeddings(ctx, workspaceID)
	var embeddable bool
	if err == nil {
		// Spaces not consented to this endpoint have no vectors from it: a search of only
		// those doesn't send the query to it either.
		err = x.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM spaces sp
			JOIN search_settings e ON e.workspace_id = sp.workspace_id
			WHERE sp.id IN (SELECT value FROM json_each(?)) AND `+embeddableSQL+`)`, in).Scan(&embeddable)
	}
	switch {
	case errors.Is(err, errNoEmbeddings):
		res.note(Notice{Code: NoticeOff}, noticeOff)
	case err != nil:
		return Results{}, err
	case !embeddable:
		res.note(Notice{Code: NoticeNoConsent}, noticeNoConsent)
	default:
		ectx, cancel := context.WithTimeout(ctx, queryTimeout)
		vs, err := x.embed(ectx, ep, e.Model, []string{q.Text})
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return Results{}, ctx.Err()
			}
			x.log.Warn("search: embedding a query failed; full text only", "err", err)
			res.note(Notice{Code: NoticeFailed}, noticeFailed)
			if errors.Is(err, egress.ErrRefused) {
				res.note(Notice{Code: NoticeRefused}, noticeRefused)
			}
		} else {
			qv = vs[0]
			res.Mode = ModeHybrid
		}
	}

	var hits []hit
	if qv == nil {
		if hits, err = x.fullText(ctx, in, match, q.Limit+1, q.Offset, true); err != nil {
			return Results{}, err
		}
		for i := range hits {
			hits[i].score = -hits[i].bm25
		}
	} else {
		// Both rankings are read to the same depth for every page, so a document's fused
		// score, and the order, don't depend on the page asked for: pages neither repeat
		// nor skip results. Snippets are made for the page's results only.
		text, err := x.fullText(ctx, in, match, hybridDepth, 0, false)
		if err != nil {
			return Results{}, err
		}
		near, err := x.nearest(ctx, workspaceID, in, e.key(), qv, hybridDepth)
		if err != nil {
			return Results{}, err
		}
		if hits, err = x.fuse(ctx, in, text, near); err != nil {
			return Results{}, err
		}
		hits = hits[min(q.Offset, len(hits)):]
		if err := x.textSnippets(ctx, match, hits[:min(len(hits), q.Limit)]); err != nil {
			return Results{}, err
		}
		var pending int
		if err := x.db.QueryRowContext(ctx, `SELECT count(*) `+pendingSQL+` AND d.space_id IN (SELECT value FROM json_each(?))`,
			in).Scan(&pending); err != nil {
			return Results{}, err
		}
		if pending > 0 {
			res.note(Notice{Code: NoticePending, Count: pending}, fmt.Sprintf(noticePending, pending))
		}
	}

	if len(hits) > q.Limit {
		hits = hits[:q.Limit]
		// No next page past the deepest offset asked for: it would be refused.
		if next := q.Offset + q.Limit; next <= MaxOffset {
			res.NextOffset = &next
		}
	}
	if err := x.passages(ctx, hits); err != nil {
		return Results{}, err
	}
	for _, h := range hits {
		res.Results = append(res.Results, Result{
			SpaceID: h.space, SpaceName: names[h.space], Path: h.path, Title: h.title, Snippet: h.snippet,
			Score: h.score, Match: h.match, Version: h.version, Commit: h.commit,
		})
	}
	if behind {
		res.Notice = strings.TrimSpace(noticeBehind + " " + res.Notice)
		res.Notices = append([]Notice{{Code: NoticeBehind}}, res.Notices...)
	}
	return res, nil
}

// checkQuery checks q, sets its default limit, and returns its FTS5 query.
func checkQuery(q *Query) (string, error) {
	switch {
	case strings.TrimSpace(q.Text) == "":
		return "", invalid(apierr.CodeSearchQueryRequired, nil, "q, what to search for, is required")
	case len(q.Text) > MaxQueryLen || !utf8.ValidString(q.Text):
		return "", invalid(apierr.CodeSearchQueryTooLong, apierr.Params{"max": MaxQueryLen}, "q is at most %d bytes of text", MaxQueryLen)
	case q.Limit < 0 || q.Limit > MaxLimit:
		return "", fmt.Errorf("%w: limit must be 1 to %d", ErrInvalid, MaxLimit)
	case q.Offset < 0 || q.Offset > MaxOffset:
		return "", fmt.Errorf("%w: offset must be 0 to %d", ErrInvalid, MaxOffset)
	}
	if q.Limit == 0 {
		q.Limit = DefaultLimit
	}
	match := matchExpr(q.Text)
	if match == "" {
		return "", invalid(apierr.CodeSearchQueryNoWords, nil, "q has no words to search for")
	}
	return match, nil
}

// matchExpr turns what someone typed into an FTS5 query: every word must match, as a
// prefix ("plan" finds "planning"), and words in double quotes as a phrase. A word in an
// unspaced script is split as the index splits it (segment), so it is a phrase of its
// characters. Only letters, digits and marks reach FTS5, each word quoted: its operators
// and column filters aren't available, so no input is a syntax error.
func matchExpr(q string) string {
	var parts []string
	tokens := 0
	add := func(words []string, prefix bool) {
		t := strings.FieldsFunc(segment(strings.Join(words, " ")), func(r rune) bool { return r == ' ' || r == sep })
		t = t[:min(len(t), maxTokens-tokens)]
		if len(t) == 0 || len(parts) == maxTerms {
			return
		}
		tokens += len(t)
		part := `"` + strings.Join(t, " ") + `"`
		if prefix {
			part += "*"
		}
		parts = append(parts, part)
	}
	for i, seg := range strings.Split(q, `"`) {
		words := strings.FieldsFunc(seg, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.Is(unicode.M, r)
		})
		switch {
		case len(words) == 0:
		case i%2 == 1:
			add(words, false)
		default:
			for _, w := range words {
				add([]string{w}, true)
			}
		}
	}
	return strings.Join(parts, " ")
}

// hit is a document found, with what each ranking said of it.
type hit struct {
	doc, space                   int64
	path, title, version, commit string
	snippet                      []Segment
	bm25                         float64 // lower is better
	score                        float64
	match                        string
	seq                          int // the passage most like the query, if found by meaning only
}

// The characters that mark matches in FTS5 snippets; cleanBody keeps them out of the text.
const (
	markStart = '\x02'
	markEnd   = '\x03'
)

// rankSQL is the full-text rank, lower first: bm25 with weights for path, title and body.
// bm25 favours short rows, and an upload's row is only its path and title, so uploads
// count half: a document named like the query comes before an upload named like it.
const rankSQL = `bm25(search_fts, 2.0, 5.0, 1.0) * CASE WHEN d.markdown THEN 1.0 ELSE 0.5 END`

// hybridDepth is how many results of each ranking a hybrid search fuses: enough for the
// deepest page.
const hybridDepth = MaxOffset + MaxLimit + 1

// snippetSQL is a document's full-text snippet, matches marked: from a Markdown document's
// text, or from an upload's path or title (it has no text), whichever matched. It is 24
// words long, or 64 (the most FTS5 allows) in a text with an unspaced script, whose words
// are single characters (segment): about as much to read.
const snippetSQL = `snippet(search_fts, CASE WHEN d.markdown THEN 2 ELSE -1 END, char(2), char(3), '…',
	CASE WHEN instr(search_fts.body, char(31)) THEN 64 ELSE 24 END)`

// fullText returns the documents of the spaces in (a JSON array of IDs) that match the
// FTS5 query match, best first, from offset, at most limit; with their snippets if
// snippets is set.
func (x *Index) fullText(ctx context.Context, in, match string, limit, offset int, snippets bool) ([]hit, error) {
	snippet := `''`
	if snippets {
		snippet = snippetSQL
	}
	rows, err := x.db.QueryContext(ctx, `SELECT d.id, d.space_id, d.path, d.title, d.version, s.commit_id,
			`+snippet+`, `+rankSQL+`
		FROM search_fts
		JOIN search_docs d ON d.id = search_fts.rowid
		JOIN search_spaces s ON s.space_id = d.space_id
		WHERE search_fts MATCH ? AND d.space_id IN (SELECT value FROM json_each(?))
		ORDER BY `+rankSQL+`, d.path, d.id LIMIT ? OFFSET ?`, match, in, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []hit
	for rows.Next() {
		h := hit{match: "text"}
		var snippet string
		if err := rows.Scan(&h.doc, &h.space, &h.path, &h.title, &h.version, &h.commit, &snippet, &h.bm25); err != nil {
			return nil, err
		}
		h.snippet = segments(snippet)
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// passages sets the snippets of the hits found by meaning only: each one's passage most
// like the query. A document changed since it was ranked may have none: its snippet is
// then empty.
func (x *Index) passages(ctx context.Context, hits []hit) error {
	var keys [][2]int64
	at := map[[2]int64]int{}
	for i, h := range hits {
		if h.match == "semantic" {
			k := [2]int64{h.doc, int64(h.seq)}
			keys = append(keys, k)
			at[k] = i
			hits[i].snippet = []Segment{}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	raw, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	rows, err := x.db.QueryContext(ctx, `SELECT doc_id, seq, text FROM search_chunks
		WHERE (doc_id, seq) IN (SELECT value ->> 0, value ->> 1 FROM json_each(?))`, string(raw))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k [2]int64
		var text string
		if err := rows.Scan(&k[0], &k[1], &text); err != nil {
			return err
		}
		hits[at[k]].snippet = passage(text)
	}
	return rows.Err()
}

// textSnippets sets the snippets of the hits full-text search found, those of a hybrid
// search's page.
func (x *Index) textSnippets(ctx context.Context, match string, hits []hit) error {
	var ids []int64
	at := map[int64]int{}
	for i, h := range hits {
		if h.match != "semantic" {
			ids = append(ids, h.doc)
			at[h.doc] = i
		}
	}
	if len(ids) == 0 {
		return nil
	}
	docs, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	rows, err := x.db.QueryContext(ctx, `SELECT d.id, `+snippetSQL+` FROM search_fts
		JOIN search_docs d ON d.id = search_fts.rowid
		WHERE search_fts MATCH ? AND search_fts.rowid IN (SELECT value FROM json_each(?))`, match, string(docs))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var snippet string
		if err := rows.Scan(&id, &snippet); err != nil {
			return err
		}
		hits[at[id]].snippet = segments(snippet)
	}
	return rows.Err()
}

// segments splits an FTS5 snippet at its marks. The characters of a phrase in an
// unspaced script are marked one by one, so with the separators between them taken out,
// neighbouring pieces are joined.
func segments(snippet string) []Segment {
	out := []Segment{}
	match := false
	snippet = unsegment(snippet)
	for len(snippet) > 0 {
		mark := markStart
		if match {
			mark = markEnd
		}
		i := strings.IndexRune(snippet, mark)
		if i < 0 {
			i = len(snippet)
		}
		switch {
		case i == 0:
		case len(out) > 0 && out[len(out)-1].Match == match:
			out[len(out)-1].Text += snippet[:i]
		default:
			out = append(out, Segment{Text: snippet[:i], Match: match})
		}
		snippet = snippet[min(i+1, len(snippet)):]
		match = !match
	}
	return out
}

// near is a document found by meaning, and its passage (seq) most like the query.
type near struct {
	doc        int64
	seq        int
	similarity float64
}

// nearest returns the documents of the spaces in (a JSON array of IDs, of workspaceID)
// whose passages, embedded with the model key, are most like qv, at most limit, best first.
// It compares the vector cache's vectors, and reads those of the spaces it doesn't have
// from SQLite.
func (x *Index) nearest(ctx context.Context, workspaceID int64, in, key string, qv []float32, limit int) ([]near, error) {
	// Only spaces consented to the endpoint the query went to: another's vectors, of an
	// endpoint changed since, aren't compared with it. Asked of SQLite on every search,
	// never cached.
	rows, err := x.db.QueryContext(ctx, `SELECT sp.id FROM spaces sp
		JOIN search_settings e ON e.workspace_id = sp.workspace_id
		WHERE sp.id IN (SELECT value FROM json_each(?)) AND sp.workspace_id = ? AND `+embeddableSQL+`
		ORDER BY sp.id`, in, workspaceID)
	if err != nil {
		return nil, err
	}
	var spaces []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		spaces = append(spaces, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	found, rest, err := x.vectors.nearest(ctx, x.db, workspaceID, spaces, key, qv)
	if err != nil {
		return nil, err
	}
	if len(rest) > 0 {
		raw, err := json.Marshal(rest)
		if err != nil {
			return nil, err
		}
		more, err := x.nearestBrute(ctx, string(raw), key, qv)
		if err != nil {
			return nil, err
		}
		found = append(found, more...)
	}
	return rank(found, limit), nil
}

// nearestBrute returns each document of the spaces in whose passages are embedded with
// the model key with its passage most like qv, unordered, reading every vector from
// SQLite: without the vector cache, or for a space that doesn't fit in it, and the
// reference the cache is tested against.
func (x *Index) nearestBrute(ctx context.Context, in, key string, qv []float32) ([]near, error) {
	rows, err := x.db.QueryContext(ctx, `SELECT c.doc_id, c.seq, c.vector FROM search_chunks c
		JOIN search_docs d ON d.id = c.doc_id
		JOIN spaces sp ON sp.id = d.space_id
		JOIN search_settings e ON e.workspace_id = sp.workspace_id
		WHERE d.space_id IN (SELECT value FROM json_each(?)) AND d.embed_key = ? AND d.embed_version = d.version
			AND `+embeddableSQL, in, key)
	if err != nil {
		return nil, err
	}
	best := map[int64]near{}
	v := make([]float32, len(qv))
	for rows.Next() {
		var doc int64
		var seq int
		var vec sql.RawBytes
		if err := rows.Scan(&doc, &seq, &vec); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if len(vec) != 4*len(qv) {
			continue // of another size
		}
		decodeVectorInto(v, vec)
		sim := float64(dot32(qv, v))
		if b, ok := best[doc]; !ok || sim > b.similarity {
			best[doc] = near{doc: doc, seq: seq, similarity: sim}
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	all := make([]near, 0, len(best))
	for _, n := range best {
		all = append(all, n)
	}
	return all, nil
}

// rank orders documents found by meaning, most alike first, and keeps the first limit.
func rank(found []near, limit int) []near {
	slices.SortFunc(found, func(a, b near) int {
		return cmp.Or(cmp.Compare(b.similarity, a.similarity), cmp.Compare(a.doc, b.doc))
	})
	return found[:min(len(found), limit)]
}

// rrfK is reciprocal rank fusion's constant: a document's score is the sum, over the
// rankings that found it, of 1/(rrfK + its rank there). 60 is the usual choice: it keeps
// one ranking's top result from outweighing agreement between both.
const rrfK = 60

// fuse merges the full-text and semantic rankings by reciprocal rank fusion, best first.
func (x *Index) fuse(ctx context.Context, in string, text []hit, sem []near) ([]hit, error) {
	byDoc := map[int64]*hit{}
	var all []*hit
	for i := range text {
		h := &text[i]
		h.score = 1.0 / float64(rrfK+i+1)
		byDoc[h.doc] = h
		all = append(all, h)
	}
	var missing []int64
	for i, n := range sem {
		if h, ok := byDoc[n.doc]; ok {
			h.score += 1.0 / float64(rrfK+i+1)
			h.match = "both"
			continue
		}
		h := &hit{doc: n.doc, seq: n.seq, score: 1.0 / float64(rrfK+i+1), match: "semantic"}
		byDoc[n.doc] = h
		all = append(all, h)
		missing = append(missing, n.doc)
	}
	if len(missing) > 0 {
		docs, err := json.Marshal(missing)
		if err != nil {
			return nil, err
		}
		rows, err := x.db.QueryContext(ctx, `SELECT d.id, d.space_id, d.path, d.title, d.version, s.commit_id
			FROM search_docs d JOIN search_spaces s ON s.space_id = d.space_id
			WHERE d.id IN (SELECT value FROM json_each(?)) AND d.space_id IN (SELECT value FROM json_each(?))`, string(docs), in)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var h hit
			if err := rows.Scan(&id, &h.space, &h.path, &h.title, &h.version, &h.commit); err != nil {
				_ = rows.Close()
				return nil, err
			}
			m := byDoc[id]
			m.space, m.path, m.title, m.version, m.commit = h.space, h.path, h.title, h.version, h.commit
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	out := make([]hit, 0, len(all))
	for _, h := range all {
		if h.path != "" { // a document gone since its vectors were read is left out
			out = append(out, *h)
		}
	}
	slices.SortStableFunc(out, func(a, b hit) int {
		return cmp.Or(cmp.Compare(b.score, a.score), strings.Compare(a.path, b.path))
	})
	return out, nil
}

// passageLen is the most characters of a passage a semantic result shows.
const passageLen = 300

// passage is a semantic result's snippet: the start of its passage most like the query.
func passage(text string) []Segment {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) > passageLen {
		text = string([]rune(text)[:passageLen]) + "…"
	}
	return []Segment{{Text: text}}
}
