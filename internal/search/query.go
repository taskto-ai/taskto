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

const (
	DefaultLimit = 20
	MaxLimit     = 100
	MaxOffset    = 1000
	MaxQueryLen  = 500
	maxTerms     = 32
	maxTokens    = 64
)

type Query struct {
	Text   string
	Limit  int
	Offset int
}

type Segment struct {
	Text  string `json:"text"`
	Match bool   `json:"match,omitempty"`
}

type Result struct {
	SpaceID   int64     `json:"space_id"`
	SpaceName string    `json:"space_name"`
	Path      string    `json:"path"`
	Title     string    `json:"title"`
	Snippet   []Segment `json:"snippet"`
	Score     float64   `json:"score"`
	Match     string    `json:"match"`
	Version   string    `json:"version"`
	Commit    string    `json:"commit"`
}

const (
	ModeFullText = "fulltext"
	ModeHybrid   = "hybrid"
)

type Results struct {
	Results    []Result `json:"results"`
	Mode       string   `json:"mode"`
	Notice     string   `json:"notice,omitempty"`
	Notices    []Notice `json:"notices,omitempty"`
	NextOffset *int     `json:"next_offset,omitempty"`
}

type Notice struct {
	Code  string `json:"code"`
	Count int    `json:"count,omitempty"`
}

const (
	NoticeOff       = "embeddings_off"
	NoticeFailed    = "embeddings_failed"
	NoticeRefused   = "egress_refused"
	NoticeNoConsent = "no_consent"
	NoticePending   = "pending"
	NoticeBehind    = "behind"
)

func (r *Results) note(n Notice, text string) {
	r.Notice, r.Notices = text, []Notice{n}
}

var ErrInvalid = errors.New("invalid search")

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

const queryTimeout = 15 * time.Second

func (x *Index) Search(ctx context.Context, c *access.Checker, userID, workspaceID int64, spaces []access.Space, q Query) (Results, error) {
	res, err := x.searchIndex(ctx, workspaceID, spaces, q)
	if err != nil {
		return Results{}, err
	}
	if x.beforeRelease != nil {
		x.beforeRelease()
	}
	return x.stillReadable(ctx, c, userID, spaces, res)
}

func (x *Index) stillReadable(ctx context.Context, c *access.Checker, userID int64, spaces []access.Space, res Results) (Results, error) {
	readable := make(map[int64]bool, len(spaces))
	err := c.Snapshot(ctx, func(snap *access.Checker, _ *sql.Tx) error {
		for _, was := range spaces {
			cur, err := snap.Check(ctx, userID, was.ID)
			switch {
			case errors.Is(err, access.ErrNotFound):
				readable[was.ID] = false
			case err != nil:
				return err
			default:
				readable[was.ID] = cur.WorkspaceID == was.WorkspaceID && (was.Generation == 0 || cur.Generation == was.Generation)
			}
		}
		return nil
	})
	if err != nil {
		return Results{}, err
	}
	lost := false
	for _, ok := range readable {
		lost = lost || !ok
	}
	if !lost {
		return res, nil
	}
	kept := res.Results[:0:0]
	for _, r := range res.Results {
		if readable[r.SpaceID] {
			kept = append(kept, r)
		}
	}
	res.Results, res.NextOffset = kept, nil
	var notices []Notice
	for _, n := range res.Notices {
		if n.Code == NoticePending {
			res.Notice = strings.TrimSpace(strings.Replace(res.Notice, fmt.Sprintf(noticePending, n.Count), "", 1))
			continue
		}
		notices = append(notices, n)
	}
	res.Notices = notices
	return res, nil
}

func (x *Index) searchIndex(ctx context.Context, workspaceID int64, spaces []access.Space, q Query) (Results, error) {
	match, err := checkQuery(&q)
	if err != nil {
		return Results{}, err
	}
	names := map[int64]string{}
	scope := []scopeEntry{}
	behind := false
	for _, sp := range spaces {
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
		scope = append(scope, scopeEntry{ID: sp.ID, Workspace: sp.WorkspaceID, Generation: sp.Generation})
	}
	if len(scope) == 0 {
		return Results{Results: []Result{}, Mode: ModeFullText}, nil
	}
	idsJSON, err := json.Marshal(scope)
	if err != nil {
		return Results{}, err
	}
	in := string(idsJSON)

	res := Results{Results: []Result{}, Mode: ModeFullText}
	var qv []float32
	e, ep, err := x.embeddings(ctx, workspaceID)
	var embeddable bool
	if err == nil {
		if x.afterQueryEndpoint != nil {
			x.afterQueryEndpoint()
		}
		err = x.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM spaces sp
			JOIN search_settings e ON e.workspace_id = sp.workspace_id
			WHERE sp.id IN (`+scopeSQL+`) AND e.account_id = ? AND e.model = ? AND `+embeddableAtSQL+`)`,
			in, e.Account, e.Model, ep.BaseURL).Scan(&embeddable)
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
			if x.afterQueryEmbedded != nil {
				x.afterQueryEmbedded()
			}
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
		text, err := x.fullText(ctx, in, match, hybridDepth, 0, false)
		if err != nil {
			return Results{}, err
		}
		near, err := x.nearest(ctx, workspaceID, in, rankEndpoint{key: e.key(), baseURL: ep.BaseURL}, qv, hybridDepth)
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
		if err := x.db.QueryRowContext(ctx, `SELECT count(*) `+pendingSQL+` AND d.space_id IN (`+scopeSQL+`)`,
			in).Scan(&pending); err != nil {
			return Results{}, err
		}
		if pending > 0 {
			res.note(Notice{Code: NoticePending, Count: pending}, fmt.Sprintf(noticePending, pending))
		}
	}

	if len(hits) > q.Limit {
		hits = hits[:q.Limit]
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

type rankEndpoint struct{ key, baseURL string }

type scopeEntry struct {
	ID         int64 `json:"id"`
	Workspace  int64 `json:"workspace"`
	Generation int64 `json:"generation"`
}

const scopeSQL = `SELECT sp.id FROM spaces sp, json_each(?) j
	WHERE sp.id = j.value ->> 'id' AND sp.workspace_id = j.value ->> 'workspace'
		AND (j.value ->> 'generation' = 0 OR sp.generation = j.value ->> 'generation')`

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

type hit struct {
	doc, space                   int64
	path, title, version, commit string
	snippet                      []Segment
	bm25                         float64
	score                        float64
	match                        string
	seq                          int
}

const (
	markStart = '\x02'
	markEnd   = '\x03'
)

const rankSQL = `bm25(search_fts, 2.0, 5.0, 1.0) * CASE WHEN d.markdown THEN 1.0 ELSE 0.5 END`

const hybridDepth = MaxOffset + MaxLimit + 1

const snippetSQL = `snippet(search_fts, CASE WHEN d.markdown THEN 2 ELSE -1 END, char(2), char(3), '…',
	CASE WHEN instr(search_fts.body, char(31)) THEN 64 ELSE 24 END)`

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
		WHERE search_fts MATCH ? AND d.space_id IN (`+scopeSQL+`)
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

type near struct {
	doc        int64
	seq        int
	similarity float64
}

func (x *Index) nearest(ctx context.Context, workspaceID int64, in string, ep rankEndpoint, qv []float32, limit int) ([]near, error) {
	key := ep.key
	rows, err := x.db.QueryContext(ctx, `SELECT sp.id, sp.generation FROM spaces sp
		JOIN search_settings e ON e.workspace_id = sp.workspace_id
		WHERE sp.id IN (`+scopeSQL+`) AND sp.workspace_id = ? AND `+keySQL+` = ? AND `+embeddableAtSQL+`
		ORDER BY sp.id`, in, workspaceID, key, ep.baseURL)
	if err != nil {
		return nil, err
	}
	var spaces []spaceRef
	for rows.Next() {
		var sp spaceRef
		if err := rows.Scan(&sp.id, &sp.generation); err != nil {
			_ = rows.Close()
			return nil, err
		}
		spaces = append(spaces, sp)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	found, rest, err := x.vectors.nearest(ctx, x.db, workspaceID, spaces, ep, qv)
	if err != nil {
		return nil, err
	}
	if len(rest) > 0 {
		var entries []scopeEntry
		var all []scopeEntry
		if err := json.Unmarshal([]byte(in), &all); err != nil {
			return nil, err
		}
		for _, e := range all {
			if slices.Contains(rest, e.ID) {
				entries = append(entries, e)
			}
		}
		raw, err := json.Marshal(entries)
		if err != nil {
			return nil, err
		}
		more, err := x.nearestBrute(ctx, string(raw), ep, qv)
		if err != nil {
			return nil, err
		}
		found = append(found, more...)
	}
	return rank(found, limit), nil
}

func (x *Index) nearestBrute(ctx context.Context, in string, ep rankEndpoint, qv []float32) ([]near, error) {
	rows, err := x.db.QueryContext(ctx, `SELECT c.doc_id, c.seq, c.vector FROM search_chunks c
		JOIN search_docs d ON d.id = c.doc_id
		JOIN spaces sp ON sp.id = d.space_id
		JOIN search_settings e ON e.workspace_id = sp.workspace_id
		WHERE d.space_id IN (`+scopeSQL+`) AND d.embed_key = ? AND d.embed_version = d.version
			AND `+keySQL+` = ? AND `+embeddableAtSQL, in, ep.key, ep.key, ep.baseURL)
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
			continue
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

func rank(found []near, limit int) []near {
	slices.SortFunc(found, func(a, b near) int {
		return cmp.Or(cmp.Compare(b.similarity, a.similarity), cmp.Compare(a.doc, b.doc))
	})
	return found[:min(len(found), limit)]
}

const rrfK = 60

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
			WHERE d.id IN (SELECT value FROM json_each(?)) AND d.space_id IN (`+scopeSQL+`)`, string(docs), in)
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
		if h.path != "" {
			out = append(out, *h)
		}
	}
	slices.SortStableFunc(out, func(a, b hit) int {
		return cmp.Or(cmp.Compare(b.score, a.score), strings.Compare(a.path, b.path))
	})
	return out, nil
}

const passageLen = 300

func passage(text string) []Segment {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) > passageLen {
		text = string([]rune(text)[:passageLen]) + "…"
	}
	return []Segment{{Text: text}}
}
