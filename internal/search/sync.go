package search

import (
	"context"
	"database/sql"
	"errors"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/knowledge"
)

const (
	// syncBatch is how many files one transaction of a sync writes, and syncBatchBytes
	// about how much of their text: a batch ends at whichever comes first, so a sync of a
	// large space never holds more than about that much in memory.
	syncBatch      = 100
	syncBatchBytes = 32 << 20
	// searchSyncWait is how long a search waits for a sync of a space already running.
	searchSyncWait = 2 * time.Second
	// maxBody is how much of a document full-text search indexes, in bytes.
	maxBody = 4 << 20
)

// indexed is a file as the index has it.
type indexed struct {
	id      int64
	version string
	format  int // the textFormat its full text was indexed in
}

// Sync brings sp's index up to main's head and returns the commit it now reflects ("" for
// a space without documents). Only sp's ID and workspace ID are used: the index reads every
// space, as the server.
func (x *Index) Sync(ctx context.Context, sp access.Space) (string, error) {
	unlock, err := x.lock(ctx, sp.ID)
	if err != nil {
		return "", err
	}
	defer unlock()
	return x.sync(ctx, sp, 0)
}

// errLargeSync is for a sync bigger than its caller would wait for.
var errLargeSync = errors.New("search: a sync bigger than allowed here")

// syncForSearch is Sync for a search, bounded: if a sync of sp is already running (a
// catch-up at startup, say), it waits for it at most x.syncWait; and it syncs at most one
// batch itself, leaving a bigger catch-up (after a large change made outside the server)
// to the background worker. Either way it reports false, and the search answers from what
// is indexed.
func (x *Index) syncForSearch(ctx context.Context, sp access.Space) (bool, error) {
	wait, cancel := context.WithTimeout(ctx, x.syncWait)
	unlock, err := x.lock(wait, sp.ID)
	cancel()
	if err != nil {
		return false, ctx.Err()
	}
	defer unlock()
	_, err = x.sync(ctx, sp, x.batchFiles)
	if errors.Is(err, errLargeSync) {
		x.changed(sp)
		return false, nil
	}
	return err == nil, err
}

// sync is Sync, with the space's sync locked; with limit > 0, it does nothing and returns
// errLargeSync if more than limit files changed.
func (x *Index) sync(ctx context.Context, sp access.Space, limit int) (string, error) {
	// The creation time stays: it tells the space from a purged one that had its ID, which the
	// knowledge base refuses by its mark when a call carries no creation time.
	sp = access.Space{ID: sp.ID, WorkspaceID: sp.WorkspaceID, Kind: sp.Kind, CreatedAt: sp.CreatedAt, Access: access.Read}

	var since string
	var partial bool
	err := x.db.QueryRowContext(ctx, `SELECT commit_id, partial FROM search_spaces WHERE space_id = ?`, sp.ID).Scan(&since, &partial)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if partial {
		since = "" // a sync stopped half-way: start over from main's files
	}
	head, err := x.kb.Head(ctx, sp)
	if err != nil {
		return "", err
	}
	if head == since {
		if head != "" {
			return head, nil
		}
		// No documents, and none indexed: nothing to do, and nothing to write.
		var any bool
		if err := x.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM search_docs WHERE space_id = ?)`, sp.ID).Scan(&any); err != nil || !any {
			return "", err
		}
	}
	diff, err := x.kb.Changes(ctx, sp, since)
	if err != nil {
		return "", err
	}

	have := map[string]indexed{}
	rows, err := x.db.QueryContext(ctx, `SELECT path, id, version, text_format FROM search_docs WHERE space_id = ?`, sp.ID)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var p string
		var d indexed
		if err := rows.Scan(&p, &d.id, &d.version, &d.format); err != nil {
			_ = rows.Close()
			return "", err
		}
		have[p] = d
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", err
	}

	// What to write: changed files whose version the index doesn't have, or has in an
	// older textFormat, and, after a full listing, the indexed files that are gone.
	var todo []knowledge.Change
	for _, c := range diff.Changes {
		if d, ok := have[c.Path]; (c.Version == "" && !ok) || (ok && d.version == c.Version && d.format == textFormat) {
			continue
		}
		todo = append(todo, c)
	}
	if diff.Full {
		listed := make(map[string]bool, len(diff.Changes))
		for _, c := range diff.Changes {
			listed[c.Path] = true
		}
		for p := range have {
			if !listed[p] {
				todo = append(todo, knowledge.Change{Path: p})
			}
		}
	}

	if limit > 0 && len(todo) > limit {
		return "", errLargeSync
	}
	embed := false
	// At least one transaction, which records the commit, even with nothing to write: a
	// space marked partial whose files are all indexed, or one never indexed whose files
	// are all gone.
	for i, first := 0, true; first || i < len(todo); first = false {
		var docs []doc
		size := 0
		for ; i < len(todo) && len(docs) < x.batchFiles && size < x.batchBytes; i++ {
			d, err := x.load(ctx, sp, diff.Head, todo[i])
			if err != nil {
				return "", err
			}
			embed = embed || d.markdown
			size += len(d.body)
			docs = append(docs, d)
		}
		last := i >= len(todo)
		if x.onBatch != nil {
			x.onBatch(len(docs), size)
		}
		err := x.write(ctx, func(tx *sql.Tx) error {
			// A sync that takes more than one transaction says so in its first: searches
			// still see what is indexed, and if it stops half-way, the next one starts
			// over from main's files rather than trust what this one left.
			if first && !last {
				if _, err := tx.ExecContext(ctx, `INSERT INTO search_spaces (space_id, commit_id, partial) VALUES (?, ?, 1)
					ON CONFLICT (space_id) DO UPDATE SET partial = 1`, sp.ID, since); err != nil {
					return err
				}
			}
			for _, d := range docs {
				if err := writeDoc(ctx, tx, sp.ID, have[d.path], d); err != nil {
					return err
				}
			}
			if !last {
				return nil
			}
			if diff.Head == "" {
				_, err := tx.ExecContext(ctx, `DELETE FROM search_spaces WHERE space_id = ?`, sp.ID)
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO search_spaces (space_id, commit_id) VALUES (?, ?)
				ON CONFLICT (space_id) DO UPDATE SET commit_id = excluded.commit_id, partial = 0, indexed_at = excluded.indexed_at`,
				sp.ID, diff.Head)
			return err
		})
		// A document changed or gone has lost its vectors.
		var written []int64
		for _, d := range docs {
			if id := have[d.path].id; id != 0 {
				written = append(written, id)
			}
		}
		x.vectors.changedDocs(sp.ID, written...)
		if err != nil {
			return "", err
		}
	}
	if embed {
		wake(x.embedWake)
	}
	return diff.Head, nil
}

// doc is a file to index, or to remove from the index if version is empty.
type doc struct {
	path, version, title, body string
	markdown                   bool
}

// load reads what the index needs of the change c at the commit head: a Markdown
// document's title and text; an upload's path only.
func (x *Index) load(ctx context.Context, sp access.Space, head string, c knowledge.Change) (doc, error) {
	d := doc{path: c.Path, version: c.Version, markdown: knowledge.IsMarkdown(c.Path)}
	if d.version == "" {
		return d, nil
	}
	d.title = fileTitle(c.Path)
	if !d.markdown {
		return d, nil
	}
	got, err := x.kb.Read(ctx, sp, c.Path, head)
	switch {
	case errors.Is(err, knowledge.ErrInvalid) || errors.Is(err, knowledge.ErrNotFound):
		// A name the knowledge base refuses (made outside the server): its path is enough.
		d.markdown = false
		return d, nil
	case err != nil:
		return doc{}, err
	}
	d.body = cleanBody(got.Content)
	d.title = docTitle(c.Path, d.body)
	return d, nil
}

// writeDoc puts d in the index, in place of cur (zero if the index doesn't have it).
func writeDoc(ctx context.Context, tx *sql.Tx, spaceID int64, cur indexed, d doc) error {
	switch {
	case d.version == "":
		if cur.id == 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM search_docs WHERE id = ?`, cur.id)
		return err
	case cur.id == 0:
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO search_docs (space_id, path, version, markdown, title, text_format)
			VALUES (?, ?, ?, ?, ?, ?) RETURNING id`, spaceID, d.path, d.version, d.markdown, d.title, textFormat).Scan(&id); err != nil {
			return err
		}
		return writeText(ctx, tx, id, d)
	case cur.version == d.version:
		// Only indexed in an older textFormat: its text again, and its vectors, which are of
		// the same version, stay.
		if _, err := tx.ExecContext(ctx, `UPDATE search_docs SET markdown = ?, title = ?, text_format = ? WHERE id = ?`,
			d.markdown, d.title, textFormat, cur.id); err != nil {
			return err
		}
		return writeText(ctx, tx, cur.id, d)
	default:
		// Vectors are only ever of a document's current version: the old ones go now, and
		// the embeddings worker makes new ones.
		if _, err := tx.ExecContext(ctx, `DELETE FROM search_chunks WHERE doc_id = ?`, cur.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE search_docs SET version = ?, markdown = ?, title = ?, text_format = ?,
			embed_version = NULL, embed_key = NULL, embed_tries = 0, embed_after = NULL WHERE id = ?`,
			d.version, d.markdown, d.title, textFormat, cur.id); err != nil {
			return err
		}
		return writeText(ctx, tx, cur.id, d)
	}
}

// writeText puts d's path, title and text in the full-text index, as row id, in place of
// what it had there, if anything: a migration to a new textFormat can have emptied it.
func writeText(ctx context.Context, tx *sql.Tx, id int64, d doc) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM search_fts WHERE rowid = ?`, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO search_fts (rowid, path, title, body) VALUES (?, ?, ?, ?)`,
		id, segment(d.path), segment(d.title), segment(d.body))
	return err
}

// cleanBody is the text full-text search indexes: valid UTF-8, at most maxBody bytes,
// without the characters that mark matches in snippets or separate characters (sep).
func cleanBody(content []byte) string {
	if len(content) > maxBody {
		content = content[:maxBody]
	}
	return strings.Map(func(r rune) rune {
		if r == markStart || r == markEnd || r == sep {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(string(content), "\uFFFD"))
}

// fileTitle is a file's name without its extension.
func fileTitle(name string) string {
	base := path.Base(name)
	if t := strings.TrimSuffix(base, path.Ext(base)); t != "" {
		return t
	}
	return base
}

// maxTitle is the longest title, in characters.
const maxTitle = 200

// docTitle is a Markdown document's title: its front matter's title, else its first
// top-level heading, else its file name.
func docTitle(name, body string) string {
	title := ""
	lines := strings.Split(body, "\n")
	start := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			if line == "---" || line == "..." {
				start = i + 1
				break
			}
			if v, ok := strings.CutPrefix(line, "title:"); ok && title == "" {
				title = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
		if start == 0 {
			title = "" // no end to the front matter: it wasn't front matter
		}
	}
	fence := ""
	for _, line := range lines[start:] {
		if title != "" {
			break
		}
		t := strings.TrimSpace(line)
		if f := codeFence(t); f != "" {
			switch {
			case fence == "":
				fence = f
			case strings.HasPrefix(t, fence):
				fence = ""
			}
			continue
		}
		if fence == "" {
			if h, ok := strings.CutPrefix(t, "# "); ok {
				title = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(h), "#"))
			}
		}
	}
	if title == "" {
		return fileTitle(name)
	}
	if utf8.RuneCountInString(title) > maxTitle {
		title = string([]rune(title)[:maxTitle])
	}
	return title
}

// codeFence returns the fence a line opens or closes (``` or ~~~), or "".
func codeFence(line string) string {
	for _, f := range []string{"```", "~~~"} {
		if strings.HasPrefix(line, f) {
			return f
		}
	}
	return ""
}
