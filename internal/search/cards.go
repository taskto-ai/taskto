package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"taskto.ai/taskto/internal/access"
)

const maxCardThread = 256 << 10

const cardBatch = 100

const cardSyncTries = 2

type CardResult struct {
	SpaceID   int64     `json:"space_id"`
	SpaceName string    `json:"space_name"`
	BoardID   int64     `json:"board_id"`
	BoardName string    `json:"board_name"`
	CardID    int64     `json:"card_id"`
	Title     string    `json:"title"`
	Column    string    `json:"column"`
	Snippet   []Segment `json:"snippet"`
}

type CardResults struct {
	Results    []CardResult `json:"results"`
	NextOffset *int         `json:"next_offset,omitempty"`
	Incomplete bool         `json:"incomplete,omitempty"`
}

const cardOrderSQL = `CASE WHEN c.id IN (SELECT rowid FROM search_card_fts WHERE search_card_fts MATCH ?) THEN 2
		WHEN c.id IN (SELECT rowid FROM search_card_fts WHERE search_card_fts MATCH ?) THEN 1 ELSE 0 END DESC,
	c.updated_at DESC, c.id DESC`

const cardSnippetSQL = `snippet(search_card_fts, -1, char(2), char(3), '…',
	CASE WHEN instr(search_card_fts.title || search_card_fts.body || search_card_fts.comments, char(31)) THEN 64 ELSE 24 END)`

func (x *Index) SearchCards(ctx context.Context, checker *access.Checker, userID, workspaceID int64, spaces []access.Space, q Query) (CardResults, error) {
	match, err := checkQuery(&q)
	if err != nil {
		return CardResults{}, err
	}
	ids := []int64{}
	for _, sp := range spaces {
		if sp.WorkspaceID != workspaceID || !sp.Access.Allows(access.Read) {
			continue
		}
		ids = append(ids, sp.ID)
	}
	res := CardResults{Results: []CardResult{}}
	if len(ids) == 0 {
		return res, nil
	}
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return CardResults{}, err
	}
	in := string(idsJSON)
	var found []CardResult
	for try := 1; ; try++ {
		if err := x.syncCards(ctx, in); err != nil {
			if ctx.Err() != nil {
				return CardResults{}, ctx.Err()
			}
			x.log.Error("search: syncing cards before a search; its results may be behind", "err", err)
		}
		if x.afterCardSync != nil {
			x.afterCardSync()
		}
		var behind bool
		if found, behind, err = x.findCards(ctx, in, match, q); err != nil {
			return CardResults{}, err
		}
		if !behind {
			break
		}
		if try == cardSyncTries {
			res.Incomplete = true
			break
		}
	}
	names := map[int64]string{}
	for _, r := range found {
		if _, ok := names[r.SpaceID]; ok {
			continue
		}
		sp, err := checker.Check(ctx, userID, r.SpaceID)
		switch {
		case errors.Is(err, access.ErrNotFound) || (err == nil && sp.WorkspaceID != workspaceID):
			names[r.SpaceID] = ""
		case err != nil:
			return CardResults{}, err
		default:
			names[r.SpaceID] = sp.Name
		}
	}
	more := len(found) > q.Limit
	if more {
		found = found[:q.Limit]
	}
	for _, r := range found {
		if names[r.SpaceID] == "" {
			continue
		}
		r.SpaceName = names[r.SpaceID]
		res.Results = append(res.Results, r)
	}
	if next := q.Offset + q.Limit; more && !res.Incomplete && next <= MaxOffset {
		res.NextOffset = &next
	}
	return res, nil
}

func (x *Index) findCards(ctx context.Context, in, match string, q Query) ([]CardResult, bool, error) {
	titleMatch := "{title} : (" + match + ")"
	bodyMatch := "{title body} : (" + match + ")"
	rows, err := x.db.QueryContext(ctx, `SELECT st.behind, p.id, p.space_id, p.board_id, p.board_name, p.title, p.col, p.snippet
		FROM (SELECT EXISTS (SELECT 1 `+queuedCardsSQL+`) AS behind) st
		LEFT JOIN (
			SELECT c.id AS id, b.space_id AS space_id, b.id AS board_id, b.name AS board_name, c.title AS title, c.col AS col,
				`+cardSnippetSQL+` AS snippet
			FROM search_card_fts
			JOIN cards c ON c.id = search_card_fts.rowid
			JOIN boards b ON b.id = c.board_id
			WHERE search_card_fts MATCH ? AND b.space_id IN (SELECT value FROM json_each(?))
				AND NOT EXISTS (SELECT 1 FROM search_card_queue WHERE card_id = c.id)
			ORDER BY `+cardOrderSQL+` LIMIT ? OFFSET ?) p ON true`,
		in, match, in, titleMatch, bodyMatch, q.Limit+1, q.Offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var found []CardResult
	var behind bool
	for rows.Next() {
		var id, space, board sql.NullInt64
		var boardName, title, col, snippet sql.NullString
		if err := rows.Scan(&behind, &id, &space, &board, &boardName, &title, &col, &snippet); err != nil {
			return nil, false, err
		}
		if !id.Valid {
			continue
		}
		found = append(found, CardResult{CardID: id.Int64, SpaceID: space.Int64, BoardID: board.Int64,
			BoardName: boardName.String, Title: title.String, Column: col.String, Snippet: segments(snippet.String)})
	}
	return found, behind, rows.Err()
}

const queuedCardsSQL = `FROM search_card_queue sq
	JOIN cards c ON c.id = sq.card_id
	JOIN boards b ON b.id = c.board_id
	WHERE b.space_id IN (SELECT value FROM json_each(?))`

const queuedCardIDsSQL = `SELECT sq.card_id
	FROM search_card_queue sq
	JOIN cards c ON c.id = sq.card_id
	JOIN boards b ON b.id = c.board_id
	WHERE sq.card_id > ? AND (? OR b.space_id IN (SELECT value FROM json_each(?)))
	ORDER BY sq.card_id LIMIT ?`

func (x *Index) queueCards(ctx context.Context) error {
	return x.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO search_card_queue (card_id)
			SELECT c.id FROM cards c LEFT JOIN search_cards s ON s.card_id = c.id
			WHERE s.card_id IS NULL OR s.text_format != ?`, textFormat)
		return err
	})
}

func (x *Index) syncCards(ctx context.Context, in string) error {
	all := in == ""
	if all {
		in = "[]"
	}
	var after int64
	for {
		rows, err := x.db.QueryContext(ctx, queuedCardIDsSQL, after, all, in, cardBatch)
		if err != nil {
			return err
		}
		var stale []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			stale = append(stale, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(stale) == 0 {
			return nil
		}
		after = stale[len(stale)-1]
		if err := x.write(ctx, func(tx *sql.Tx) error {
			for _, id := range stale {
				if err := indexCard(ctx, tx, id); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
}

func indexCard(ctx context.Context, tx *sql.Tx, id int64) error {
	var title, body string
	err := tx.QueryRowContext(ctx, `SELECT title, body FROM cards WHERE id = ?`, id).Scan(&title, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT body FROM card_comments WHERE card_id = ? ORDER BY id DESC`, id)
	if err != nil {
		return err
	}
	var thread []string
	size := 0
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			_ = rows.Close()
			return err
		}
		if size += len(text); size > maxCardThread && len(thread) > 0 {
			break
		}
		thread = append(thread, cleanBody([]byte(text)))
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	slices.Reverse(thread)
	comments := strings.Join(thread, "\n")
	if _, err := tx.ExecContext(ctx, `DELETE FROM search_card_fts WHERE rowid = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO search_card_fts (rowid, title, body, comments) VALUES (?, ?, ?, ?)`,
		id, segment(cleanBody([]byte(title))), segment(cleanBody([]byte(body))), segment(comments)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO search_cards (card_id, text_format) VALUES (?, ?)
		ON CONFLICT (card_id) DO UPDATE SET text_format = excluded.text_format`, id, textFormat); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM search_card_queue WHERE card_id = ?`, id)
	return err
}
