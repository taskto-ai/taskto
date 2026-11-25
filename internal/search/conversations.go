package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"taskto.ai/taskto/internal/conversations"
)

// Conversations. People's conversations with the agent are searched in full text only, and
// apart from the rest: the title of a conversation and what was said in it (the user's and the
// agent's messages, as the conversations package exposes the agent runtime's transcript:
// conversations.Service.Transcript), and a search finds the caller's own conversations, in
// the workspace it is made in, and no one else's: a conversation is private to its owner, as
// every route to it says (conversations.Service). Semantic search does not take them in: that
// needs consent of its own, since their text would go to the embeddings endpoint.
//
// The transcript is the agent runtime's, which the server doesn't see change: it knows when a
// message is sent, when its turn ends (conversations.Service marks the conversation updated at
// both), and when a conversation is renamed, archived or removed. Triggers (migration
// search_records_conversations) queue a conversation, in the transaction that marks it, and a
// search first indexes again the caller's queued conversations (syncConversations), newest
// first, each from a call to the agent runtime made outside any transaction, and written
// together with its taking off the queue only if no other mark came meanwhile (the conversation's
// revision, which only goes up, checked in the same transaction). A conversation in the queue is never quoted, since its indexed text may be old: a
// search that finds some behind tries once more, and says its results may be incomplete if it
// still does. A conversation that is removed takes its index with it, by the schema; one the
// agent runtime no longer has leaves the index too, and is found by its title no more.
//
// Archived conversations are searched too, as the owner can read them: a result says so, read
// from the conversation as it is when the search is made. Conversations are ordered by whether
// the title matches every word, then by when they were updated last, never by a score.

// ConversationSource is where the index gets a conversation's transcript: *conversations.Service.
type ConversationSource interface {
	// Transcript returns what was said in the conversation with the agent runtime's ID
	// agentID; conversations.ErrGone if the agent runtime doesn't have it, and another error
	// if it can't say now.
	Transcript(ctx context.Context, agentID int64) (string, error)
}

// SetConversations tells the index where the conversations' transcripts are. Until it is
// called, no conversation is searched (a server with no agent runtime has none to search).
func (x *Index) SetConversations(src ConversationSource) {
	x.conversations.Store(&src)
}

func (x *Index) conversationSource() ConversationSource {
	if p := x.conversations.Load(); p != nil {
		return *p
	}
	return nil
}

// maxConversationText is the most text of a conversation the index holds, in bytes: the newest
// part of it. Older passages past it are not searched.
const maxConversationText = 256 << 10

// conversationBatch is how many queued conversations a sync reads at a time.
const conversationBatch = 20

// conversationSyncMax is the most conversations a search indexes again before it answers; the
// rest are left for the next search and the background sync, and the answer says it may be
// incomplete.
const conversationSyncMax = 60

// conversationSyncBudget is how long a search spends indexing before it answers.
const conversationSyncBudget = 20 * time.Second

// conversationSyncTries is how many times a search indexes the conversations it looks in, and
// looks again, before it says its results may be incomplete.
const conversationSyncTries = 2

// ConversationResult is a conversation found.
type ConversationResult struct {
	ConversationID int64  `json:"conversation_id"`
	Title          string `json:"title"`
	// Archived says the owner has archived it.
	Archived  bool   `json:"archived"`
	UpdatedAt string `json:"updated_at"`
	// Snippet is the passage of what was said that matched, as plain text in pieces: never
	// markup. It is empty if only the title matched.
	Snippet []Segment `json:"snippet"`
}

// ConversationResults are a page of conversations found, best first.
type ConversationResults struct {
	Results []ConversationResult `json:"results"`
	// NextOffset is the next page's offset, if there are more results; never with Incomplete.
	NextOffset *int `json:"next_offset,omitempty"`
	// Incomplete is true when conversations changed that the index couldn't catch up with (or
	// the agent runtime wasn't there to read them from): those are left out, and a later
	// search finds them. Such a reply has no NextOffset.
	Incomplete bool `json:"incomplete,omitempty"`
}

// conversationOrderSQL puts first the conversations whose title has every word of the query,
// then the others, each by when they were updated last: it depends on a conversation's own
// text and time and on nothing of any other, so nothing someone else wrote moves a result.
const conversationOrderSQL = `CASE WHEN c.id IN (SELECT rowid FROM search_conversation_fts WHERE search_conversation_fts MATCH ?) THEN 1 ELSE 0 END DESC,
	c.updated_at DESC, c.id DESC`

// conversationSnippetSQL is the snippet of what was said, matches marked: 24 words long, or
// 64 in a text with an unspaced script (see snippetSQL). Empty if only the title matched.
const conversationSnippetSQL = `snippet(search_conversation_fts, 1, char(2), char(3), '…',
	CASE WHEN instr(search_conversation_fts.body, char(31)) THEN 64 ELSE 24 END)`

// ownConversationSQL is the condition on a row c of conversations that makes it userID's, in
// workspaceID, who must be a member there: the condition every route to a conversation makes
// (conversations.Service.find), so a search finds nothing a route would refuse.
const ownConversationSQL = `c.workspace_id = ? AND c.owner_id = ?
	AND EXISTS (SELECT 1 FROM memberships m WHERE m.workspace_id = c.workspace_id AND m.user_id = c.owner_id)`

// SearchConversations finds the conversations matching q that userID owns in workspaceID, and
// no one else's, as a member of it. The ownership is part of the statement that finds them, so
// what is found is what userID owns at that moment.
func (x *Index) SearchConversations(ctx context.Context, userID, workspaceID int64, q Query) (ConversationResults, error) {
	match, err := checkQuery(&q)
	if err != nil {
		return ConversationResults{}, err
	}
	res := ConversationResults{Results: []ConversationResult{}}
	src := x.conversationSource()
	if src == nil {
		return res, nil
	}
	var found []ConversationResult
	for try := 1; ; try++ {
		incomplete, err := x.syncConversations(ctx, src, userID, workspaceID, conversationSyncMax, conversationSyncBudget)
		if err != nil {
			if ctx.Err() != nil {
				return ConversationResults{}, ctx.Err()
			}
			// What is indexed is still searched: some conversations may be missing.
			x.log.Warn("search: indexing conversations before a search; its results may be behind", "err", err)
		}
		var behind bool
		if found, behind, err = x.findConversations(ctx, userID, workspaceID, match, q); err != nil {
			return ConversationResults{}, err
		}
		if !behind && !incomplete {
			break
		}
		if try == conversationSyncTries {
			res.Incomplete = true
			break
		}
	}
	more := len(found) > q.Limit
	if more {
		found = found[:q.Limit]
	}
	res.Results = append(res.Results, found...)
	// No next page past the deepest offset asked for: it would be refused. And none after an
	// incomplete one: an offset counts the conversations indexed now.
	if next := q.Offset + q.Limit; more && !res.Incomplete && next <= MaxOffset {
		res.NextOffset = &next
	}
	return res, nil
}

// findConversations returns one page of userID's conversations in workspaceID that match the
// FTS5 query match, best first, and one more if a page follows; and whether any conversation of
// theirs was in the queue. A queued conversation is left out of the page, so a page found while
// some were queued may be short, or empty. Both come from one statement, so they are of one
// state of the database.
func (x *Index) findConversations(ctx context.Context, userID, workspaceID int64, match string, q Query) ([]ConversationResult, bool, error) {
	titleMatch := "{title} : (" + match + ")"
	rows, err := x.db.QueryContext(ctx, `SELECT st.behind, p.id, p.title, p.archived, p.updated_at, p.snippet, p.title_first
		FROM (SELECT EXISTS (SELECT 1 FROM search_conversation_queue sq JOIN conversations c ON c.id = sq.conversation_id
			WHERE `+ownConversationSQL+`) AS behind) st
		LEFT JOIN (
			SELECT c.id AS id, c.title AS title, c.archived_at IS NOT NULL AS archived, c.updated_at AS updated_at,
				c.id IN (SELECT rowid FROM search_conversation_fts WHERE search_conversation_fts MATCH ?) AS title_first,
				`+conversationSnippetSQL+` AS snippet
			FROM search_conversation_fts
			JOIN conversations c ON c.id = search_conversation_fts.rowid
			WHERE search_conversation_fts MATCH ? AND `+ownConversationSQL+`
				AND NOT EXISTS (SELECT 1 FROM search_conversation_queue WHERE conversation_id = c.id)
			ORDER BY `+conversationOrderSQL+` LIMIT ? OFFSET ?) p ON true
		ORDER BY p.title_first DESC, p.updated_at DESC, p.id DESC`,
		workspaceID, userID, titleMatch, match, workspaceID, userID, titleMatch, q.Limit+1, q.Offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var found []ConversationResult
	var behind bool
	for rows.Next() {
		var id sql.NullInt64
		var title, updatedAt, snippet sql.NullString
		var archived, titleFirst sql.NullBool
		if err := rows.Scan(&behind, &id, &title, &archived, &updatedAt, &snippet, &titleFirst); err != nil {
			return nil, false, err
		}
		if !id.Valid {
			continue
		}
		found = append(found, ConversationResult{ConversationID: id.Int64, Title: title.String, Archived: archived.Bool,
			UpdatedAt: updatedAt.String, Snippet: segments(snippet.String)})
	}
	return found, behind, rows.Err()
}

// queueConversations puts in the queue the conversations that were never indexed, or in an
// older textFormat, and those marked within the last day: a turn that was running when the
// server stopped, whose end it never saw. Conversations older than that are only indexed
// again when something marks them.
func (x *Index) queueConversations(ctx context.Context) error {
	since := x.now().Add(-24 * time.Hour).UTC().Format(timeFormat)
	return x.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO search_conversation_queue (conversation_id)
			SELECT c.id FROM conversations c LEFT JOIN search_conversations s ON s.conversation_id = c.id
			WHERE s.conversation_id IS NULL OR s.text_format != ? OR c.updated_at >= ?
			ON CONFLICT (conversation_id) DO NOTHING`, textFormat, since)
		return err
	})
}

// syncConversationsInBackground indexes the queue's conversations, everyone's, before anyone
// searches them. An agent runtime that isn't there (off, or still starting) is no failure of
// the index: the conversations stay queued, and a search finds them behind.
func (x *Index) syncConversationsInBackground(ctx context.Context) {
	src := x.conversationSource()
	if src == nil {
		return
	}
	if _, err := x.syncConversations(ctx, src, 0, 0, 0, 0); err != nil && ctx.Err() == nil {
		if errors.Is(err, conversations.ErrNoTranscript) {
			x.log.Debug("search: the agent runtime can't give conversations' transcripts now", "err", err)
			return
		}
		x.log.Error("search: indexing conversations", "err", err)
	}
}

// queuedConversation is a conversation waiting to be indexed again.
type queuedConversation struct {
	id, agentID, revision int64
}

// syncConversations indexes again queued conversations, newest first: userID's in workspaceID,
// or everyone's if userID is 0, at most limit of them and for at most budget (0: no limit). It
// stops at the first the agent runtime can't answer for (it isn't there, or it is starting),
// whose error it returns: the others would not be answered either. leftOver is true if queued
// conversations of the scope remain that it didn't index, which it left for the next sync.
func (x *Index) syncConversations(ctx context.Context, src ConversationSource, userID, workspaceID int64, limit int, budget time.Duration) (leftOver bool, err error) {
	if budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	everyone := userID == 0
	skip := []int64{}
	for done := 0; ; {
		if limit > 0 && done >= limit {
			return true, nil
		}
		skipJSON, err := json.Marshal(skip)
		if err != nil {
			return false, err
		}
		batch, err := x.queuedConversations(ctx, everyone, userID, workspaceID, string(skipJSON))
		if err != nil {
			return false, err
		}
		if len(batch) == 0 {
			return false, nil
		}
		for _, qc := range batch {
			if limit > 0 && done >= limit {
				return true, nil
			}
			done++
			// The transcript is read outside any transaction: the agent runtime may be slow.
			text, err := src.Transcript(ctx, qc.agentID)
			gone := errors.Is(err, conversations.ErrGone)
			if err != nil && !gone {
				if ctx.Err() != nil && budget > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return true, nil // the time for it is up: the rest is for the next sync
				}
				return true, err
			}
			if err := x.write(ctx, func(tx *sql.Tx) error { return indexConversation(ctx, tx, qc, text, gone) }); err != nil {
				return true, err
			}
		}
		// A conversation indexed but marked meanwhile is queued still: don't read it again in
		// this sync.
		for _, qc := range batch {
			skip = append(skip, qc.id)
		}
	}
}

// queuedConversations reads the next conversations in the queue of the scope, newest first,
// leaving out those in skip (a JSON array of IDs).
func (x *Index) queuedConversations(ctx context.Context, everyone bool, userID, workspaceID int64, skip string) ([]queuedConversation, error) {
	rows, err := x.db.QueryContext(ctx, `SELECT sq.conversation_id, c.agent_id, rv.revision
		FROM search_conversation_queue sq
		JOIN conversations c ON c.id = sq.conversation_id
		JOIN search_conversation_revisions rv ON rv.conversation_id = sq.conversation_id
		WHERE (? OR (c.workspace_id = ? AND c.owner_id = ?))
			AND sq.conversation_id NOT IN (SELECT value FROM json_each(?))
		ORDER BY c.updated_at DESC, c.id DESC LIMIT ?`, everyone, workspaceID, userID, skip, conversationBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []queuedConversation
	for rows.Next() {
		var qc queuedConversation
		if err := rows.Scan(&qc.id, &qc.agentID, &qc.revision); err != nil {
			return nil, err
		}
		out = append(out, qc)
	}
	return out, rows.Err()
}

// indexConversation puts the conversation, with the text read from the transcript, in the
// index, and takes it off the queue, only if it hasn't been marked since the transcript was read:
// its revision, which only goes up, is still qc's, checked in this transaction before anything is
// written. If it was marked, what was read is old, and whoever read after the mark (or the next
// sync) writes the index: an older read never replaces a newer one's text. A conversation the
// agent runtime no longer has (gone) leaves the index. One that was removed meanwhile is left alone.
func indexConversation(ctx context.Context, tx *sql.Tx, qc queuedConversation, text string, gone bool) error {
	var title string
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT c.title, rv.revision FROM conversations c
		JOIN search_conversation_revisions rv ON rv.conversation_id = c.id WHERE c.id = ?`, qc.id).Scan(&title, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if revision != qc.revision {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM search_conversation_fts WHERE rowid = ?`, qc.id); err != nil {
		return err
	}
	if gone {
		if _, err := tx.ExecContext(ctx, `DELETE FROM search_conversations WHERE conversation_id = ?`, qc.id); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `INSERT INTO search_conversation_fts (rowid, title, body) VALUES (?, ?, ?)`,
			qc.id, segment(cleanBody([]byte(title))), segment(cleanBody([]byte(newest(text, maxConversationText))))); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO search_conversations (conversation_id, text_format) VALUES (?, ?)
			ON CONFLICT (conversation_id) DO UPDATE SET text_format = excluded.text_format`, qc.id, textFormat); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM search_conversation_queue WHERE conversation_id = ?`, qc.id)
	return err
}

// newest is the last at most n bytes of s, starting at a line, or at least at a character.
func newest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		return s[i+1:]
	}
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}
