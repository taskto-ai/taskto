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
// The boards are the truth. Triggers (migration search_cards) queue a card, in the
// transaction that changes it, when its title or body changes or its thread does (a
// comment added, edited or deleted), and a search first indexes the queue's cards of the
// spaces it looks in (syncCards), each in a transaction of its own that reads the card and
// writes what it read and takes it off the queue. A card in the queue is never quoted, since
// its indexed text may be old: a search that finds some behind tries once more, and says
// its results may be incomplete if it still does. A card that is deleted, with its board or
// space, takes its index with it, by the schema. So the index is right whatever wrote the
// cards, and a crash leaves nothing behind.

// maxCardThread is the most text of a card's comments the index holds, in bytes; the newest
// comments come first, and the older ones past it are not searched.
const maxCardThread = 256 << 10

// cardBatch is how many cards one transaction of a sync indexes.
const cardBatch = 100

// cardSyncTries is how many times a search indexes the cards it looks in, and looks again,
// before it says its results may be incomplete.
const cardSyncTries = 2

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
}

// CardResults are a page of cards found, best first.
type CardResults struct {
	Results []CardResult `json:"results"`
	// NextOffset is the next page's offset, if there are more results.
	NextOffset *int `json:"next_offset,omitempty"`
	// Incomplete is true when cards changed that the index couldn't catch up with: those
	// are left out, and a later search finds them.
	Incomplete bool `json:"incomplete,omitempty"`
}

// The order of cards found depends on each card's own text and when it changed, and on
// nothing of any other card: a ranking with statistics of the whole index, such as bm25's,
// would let cards the caller can't read change what they can. A card is in one of three
// tiers: every word of the query in its title, every word in its title and body, or the
// rest (some word only in its thread, or split between them); the tiers go first to last,
// and within one the card changed last goes first.
const cardOrderSQL = `CASE WHEN c.id IN (SELECT rowid FROM search_card_fts WHERE search_card_fts MATCH ?) THEN 2
		WHEN c.id IN (SELECT rowid FROM search_card_fts WHERE search_card_fts MATCH ?) THEN 1 ELSE 0 END DESC,
	c.updated_at DESC, c.id DESC`

// cardSnippetSQL is the card's snippet, matches marked, from whichever of its title, body
// and thread matched best. It is 24 words long, or 64 in a text with an unspaced script
// (see snippetSQL).
const cardSnippetSQL = `snippet(search_card_fts, -1, char(2), char(3), '…',
	CASE WHEN instr(search_card_fts.title || search_card_fts.body || search_card_fts.comments, char(31)) THEN 64 ELSE 24 END)`

// SearchCards finds the cards matching q in the boards of spaces, which must be of
// workspaceID and readable by userID, as for Search: the space RequireSpace checked, or the
// user's Readable spaces. A board is read by whoever reads its space, and so are its
// cards, archived or not.
//
// Indexing takes time, and who may read a space can change meanwhile: so after it, and
// after the cards are found, each space of the results is checked again with checker, and a
// card of a space userID no longer reads is left out.
func (x *Index) SearchCards(ctx context.Context, checker *access.Checker, userID, workspaceID int64, spaces []access.Space, q Query) (CardResults, error) {
	match, err := checkQuery(&q)
	if err != nil {
		return CardResults{}, err
	}
	ids := []int64{}
	for _, sp := range spaces {
		// Never anything else, whatever the caller passes.
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
			// What is indexed is still searched: some cards may be missing.
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
	// Who may read what is asked once more, after all that took time. A space that is gone
	// from the user, or the user from the workspace, takes its cards from the results.
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
	for _, r := range found {
		if names[r.SpaceID] == "" {
			continue
		}
		r.SpaceName = names[r.SpaceID]
		res.Results = append(res.Results, r)
	}
	if len(found) > q.Limit {
		// One more than asked for was read, to know whether a page follows.
		if len(res.Results) > q.Limit {
			res.Results = res.Results[:q.Limit]
		}
		// No next page past the deepest offset asked for: it would be refused.
		if next := q.Offset + q.Limit; next <= MaxOffset {
			res.NextOffset = &next
		}
	}
	return res, nil
}

// findCards returns one page of the cards, in the spaces in (a JSON array of IDs), that
// match the FTS5 query match, best first, and one more if a page follows; and whether any
// card of those spaces was in the queue. A card whose indexed text may be old, since it is
// in the queue, is left out of the page, so a page found while some were queued may be
// short, or empty. Both come from one statement, so they are of one state of the database
// (a plain read, never a transaction): a card queued at the moment of the page is never
// missed by the flag, even when the page has no rows.
func (x *Index) findCards(ctx context.Context, in, match string, q Query) ([]CardResult, bool, error) {
	// The query's words in the title only, and in the title and body only.
	titleMatch := "{title} : (" + match + ")"
	bodyMatch := "{title body} : (" + match + ")"
	// The flag is a row of its own that the page is joined to, so it is there with no page.
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

// queuedCardsSQL is the queued cards of the spaces in (a JSON array of IDs), found by the
// boards' index by space and the queue's primary key: never a walk of every card.
const queuedCardsSQL = `FROM search_card_queue sq
	JOIN cards c ON c.id = sq.card_id
	JOIN boards b ON b.id = c.board_id
	WHERE b.space_id IN (SELECT value FROM json_each(?))`

// queuedCardIDsSQL is a batch of the queue's cards after an ID, of every space or of those
// in a JSON array: it starts from the queue, never from every card.
const queuedCardIDsSQL = `SELECT sq.card_id
	FROM search_card_queue sq
	JOIN cards c ON c.id = sq.card_id
	JOIN boards b ON b.id = c.board_id
	WHERE sq.card_id > ? AND (? OR b.space_id IN (SELECT value FROM json_each(?)))
	ORDER BY sq.card_id LIMIT ?`

// queueCards puts in the queue the cards that were never indexed, or in an older
// textFormat: what the migration, or a change of the format, leaves.
func (x *Index) queueCards(ctx context.Context) error {
	return x.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO search_card_queue (card_id)
			SELECT c.id FROM cards c LEFT JOIN search_cards s ON s.card_id = c.id
			WHERE s.card_id IS NULL OR s.text_format != ?`, textFormat)
		return err
	})
}

// syncCards indexes again the queued cards of the spaces in (a JSON array of IDs; every
// space if it is empty), a batch at a time.
func (x *Index) syncCards(ctx context.Context, in string) error {
	all := in == ""
	if all {
		in = "[]"
	}
	// Batches go by ID, each from where the last stopped, so a card changed again meanwhile
	// can't keep a sync going: the search that wanted it finds it behind.
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

// indexCard puts the card as it is now, with its thread, in the index. A card that is gone
// is left out.
func indexCard(ctx context.Context, tx *sql.Tx, id int64) error {
	var title, body string
	err := tx.QueryRowContext(ctx, `SELECT title, body FROM cards WHERE id = ?`, id).Scan(&title, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// The thread, newest comments first up to maxCardThread bytes.
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
	// Read and written in this transaction, so a change made after it queues the card again.
	_, err = tx.ExecContext(ctx, `DELETE FROM search_card_queue WHERE card_id = ?`, id)
	return err
}
