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

// Cards. Board cards are searched in full text only, and apart from documents: a card's
// title, body and thread are indexed, and a search finds the cards of the boards in the
// spaces the caller may read, as Search does for documents. Semantic search does not take
// cards in: that needs consent of its own, since a card's text would go to the embeddings
// endpoint.
//
// The boards are the truth. The index keeps, for each card, the version of the card and
// the newest comment it reflects (search_cards), and a search first brings the cards of the
// spaces it looks in up to date (syncCards): a card created, edited, commented on, moved
// or archived since is indexed again, in a transaction of its own that reads the card
// and writes what it read. A card that is deleted, with its board or space, takes its index
// with it, by the schema. So the index is right whatever wrote the cards, and a crash
// leaves nothing behind.

// maxCardThread is the most text of a card's comments the index holds, in bytes; the newest
// comments come first, and the older ones past it are not searched.
const maxCardThread = 256 << 10

// cardBatch is how many cards one transaction of a sync indexes.
const cardBatch = 100

// CardResult is a card found.
type CardResult struct {
	SpaceID   int64  `json:"space_id"`
	SpaceName string `json:"space_name"`
	BoardID   int64  `json:"board_id"`
	BoardName string `json:"board_name"`
	CardID    int64  `json:"card_id"`
	Title     string `json:"title"`
	// Column is where the card is on its board, "archived" if it is off it.
	Column string `json:"column"`
	// Snippet is the passage of the card, or of its thread, that matched, as plain text in
	// pieces: never markup.
	Snippet []Segment `json:"snippet"`
	// Score orders the results, higher first. It compares results of one search only.
	Score float64 `json:"score"`
}

// CardResults are a page of cards found.
type CardResults struct {
	Results []CardResult `json:"results"`
	// NextOffset is the next page's offset, if there are more results.
	NextOffset *int `json:"next_offset,omitempty"`
}

// cardRankSQL is the full-text rank, lower first: bm25 with weights for title, body and
// thread, so a card named like the query comes before one that only mentions it.
const cardRankSQL = `bm25(search_card_fts, 5.0, 1.0, 0.5)`

// cardSnippetSQL is the card's snippet, matches marked, from whichever of its title, body
// and thread matched best. It is 24 words long, or 64 in a text with an unspaced script
// (see snippetSQL).
const cardSnippetSQL = `snippet(search_card_fts, -1, char(2), char(3), '…',
	CASE WHEN instr(search_card_fts.title || search_card_fts.body || search_card_fts.comments, char(31)) THEN 64 ELSE 24 END)`

// SearchCards finds the cards matching q in the boards of spaces, which must be of
// workspaceID and readable by the caller, as for Search: the space RequireSpace checked, or
// the caller's Readable spaces. A board is read by whoever reads its space, and so are its
// cards, archived or not.
func (x *Index) SearchCards(ctx context.Context, workspaceID int64, spaces []access.Space, q Query) (CardResults, error) {
	match, err := checkQuery(&q)
	if err != nil {
		return CardResults{}, err
	}
	names := map[int64]string{}
	ids := []int64{}
	for _, sp := range spaces {
		// Never anything else, whatever the caller passes.
		if sp.WorkspaceID != workspaceID || !sp.Access.Allows(access.Read) {
			continue
		}
		names[sp.ID] = sp.Name
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
	if err := x.syncCards(ctx, in); err != nil {
		if ctx.Err() != nil {
			return CardResults{}, ctx.Err()
		}
		// What is indexed is still searched: some cards may be missing or out of date.
		x.log.Error("search: syncing cards before a search; its results may be behind", "err", err)
	}
	// One more than asked for, to know whether a page follows.
	rows, err := x.db.QueryContext(ctx, `SELECT c.id, b.space_id, b.id, b.name, c.title, c.col,
			`+cardSnippetSQL+`, `+cardRankSQL+`
		FROM search_card_fts
		JOIN cards c ON c.id = search_card_fts.rowid
		JOIN boards b ON b.id = c.board_id
		WHERE search_card_fts MATCH ? AND b.space_id IN (SELECT value FROM json_each(?))
		ORDER BY `+cardRankSQL+`, c.id LIMIT ? OFFSET ?`, match, in, q.Limit+1, q.Offset)
	if err != nil {
		return CardResults{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var r CardResult
		var snippet string
		var bm25 float64
		if err := rows.Scan(&r.CardID, &r.SpaceID, &r.BoardID, &r.BoardName, &r.Title, &r.Column, &snippet, &bm25); err != nil {
			return CardResults{}, err
		}
		r.SpaceName, r.Snippet, r.Score = names[r.SpaceID], segments(snippet), -bm25
		res.Results = append(res.Results, r)
	}
	if err := rows.Err(); err != nil {
		return CardResults{}, err
	}
	if len(res.Results) > q.Limit {
		res.Results = res.Results[:q.Limit]
		// No next page past the deepest offset asked for: it would be refused.
		if next := q.Offset + q.Limit; next <= MaxOffset {
			res.NextOffset = &next
		}
	}
	return res, nil
}

// syncCards indexes again the cards, of the spaces in (a JSON array of IDs; every space if
// it is empty), that the index doesn't reflect: those not indexed, indexed in an older
// textFormat, or changed since, in a card's version or its newest comment.
func (x *Index) syncCards(ctx context.Context, in string) error {
	all := in == ""
	if all {
		in = "[]"
	}
	// Batches go by ID, each from where the last stopped, so a long backlog is scanned once.
	var after int64
	for {
		rows, err := x.db.QueryContext(ctx, `SELECT c.id FROM cards c
			JOIN boards b ON b.id = c.board_id
			LEFT JOIN search_cards s ON s.card_id = c.id
			WHERE c.id > ? AND (? OR b.space_id IN (SELECT value FROM json_each(?)))
				AND (s.card_id IS NULL OR s.version != c.version OR s.text_format != ?
					OR s.comment_id != coalesce((SELECT max(id) FROM card_comments WHERE card_id = c.id), 0))
			ORDER BY c.id LIMIT ?`, after, all, in, textFormat, cardBatch)
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

// indexCard puts the card as it is now, with its thread, in the index. A card that is gone
// is left out.
func indexCard(ctx context.Context, tx *sql.Tx, id int64) error {
	var title, body string
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT title, body, version FROM cards WHERE id = ?`, id).Scan(&title, &body, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// The thread, newest comments first up to maxCardThread bytes, and the newest one's ID.
	rows, err := tx.QueryContext(ctx, `SELECT id, body FROM card_comments WHERE card_id = ? ORDER BY id DESC`, id)
	if err != nil {
		return err
	}
	var newest int64
	var thread []string
	size := 0
	for rows.Next() {
		var cid int64
		var text string
		if err := rows.Scan(&cid, &text); err != nil {
			_ = rows.Close()
			return err
		}
		if newest == 0 {
			newest = cid
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
	_, err = tx.ExecContext(ctx, `INSERT INTO search_cards (card_id, version, comment_id, text_format) VALUES (?, ?, ?, ?)
		ON CONFLICT (card_id) DO UPDATE SET version = excluded.version, comment_id = excluded.comment_id,
			text_format = excluded.text_format`, id, version, newest, textFormat)
	return err
}
