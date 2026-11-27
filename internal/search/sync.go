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
	syncBatch      = 100
	syncBatchBytes = 32 << 20
	searchSyncWait = 2 * time.Second
	maxBody        = 4 << 20
)

type indexed struct {
	id      int64
	version string
	format  int
}

func (x *Index) Sync(ctx context.Context, sp access.Space) (string, error) {
	unlock, err := x.lock(ctx, sp.ID)
	if err != nil {
		return "", err
	}
	defer unlock()
	return x.sync(ctx, sp, 0)
}

var errLargeSync = errors.New("search: a sync bigger than allowed here")

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

func (x *Index) sync(ctx context.Context, sp access.Space, limit int) (string, error) {
	sp = access.Space{ID: sp.ID, WorkspaceID: sp.WorkspaceID, Kind: sp.Kind, Access: access.Read}

	var since string
	var partial bool
	err := x.db.QueryRowContext(ctx, `SELECT commit_id, partial FROM search_spaces WHERE space_id = ?`, sp.ID).Scan(&since, &partial)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if partial {
		since = ""
	}
	head, err := x.kb.Head(ctx, sp)
	if err != nil {
		return "", err
	}
	if head == since {
		if head != "" {
			return head, nil
		}
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

type doc struct {
	path, version, title, body string
	markdown                   bool
}

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
		d.markdown = false
		return d, nil
	case err != nil:
		return doc{}, err
	}
	d.body = cleanBody(got.Content)
	d.title = docTitle(c.Path, d.body)
	return d, nil
}

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
		if _, err := tx.ExecContext(ctx, `UPDATE search_docs SET markdown = ?, title = ?, text_format = ? WHERE id = ?`,
			d.markdown, d.title, textFormat, cur.id); err != nil {
			return err
		}
		return writeText(ctx, tx, cur.id, d)
	default:
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

func writeText(ctx context.Context, tx *sql.Tx, id int64, d doc) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM search_fts WHERE rowid = ?`, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO search_fts (rowid, path, title, body) VALUES (?, ?, ?, ?)`,
		id, segment(d.path), segment(d.title), segment(d.body))
	return err
}

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

func fileTitle(name string) string {
	base := path.Base(name)
	if t := strings.TrimSuffix(base, path.Ext(base)); t != "" {
		return t
	}
	return base
}

const maxTitle = 200

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
			title = ""
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

func codeFence(line string) string {
	for _, f := range []string{"```", "~~~"} {
		if strings.HasPrefix(line, f) {
			return f
		}
	}
	return ""
}
