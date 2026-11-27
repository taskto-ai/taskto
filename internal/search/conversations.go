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

type ConversationSource interface {
	Transcript(ctx context.Context, agentID int64) (string, error)
}

func (x *Index) SetConversations(src ConversationSource) {
	x.conversations.Store(&src)
}

func (x *Index) conversationSource() ConversationSource {
	if p := x.conversations.Load(); p != nil {
		return *p
	}
	return nil
}

const maxConversationText = 256 << 10

const conversationBatch = 20

const conversationSyncMax = 60

const conversationSyncBudget = 20 * time.Second

const conversationSyncTries = 2

type ConversationResult struct {
	ConversationID int64     `json:"conversation_id"`
	Title          string    `json:"title"`
	Archived       bool      `json:"archived"`
	UpdatedAt      string    `json:"updated_at"`
	Snippet        []Segment `json:"snippet"`
}

type ConversationResults struct {
	Results    []ConversationResult `json:"results"`
	NextOffset *int                 `json:"next_offset,omitempty"`
	Incomplete bool                 `json:"incomplete,omitempty"`
}

const conversationOrderSQL = `CASE WHEN c.id IN (SELECT rowid FROM search_conversation_fts WHERE search_conversation_fts MATCH ?) THEN 1 ELSE 0 END DESC,
	c.updated_at DESC, c.id DESC`

const conversationSnippetSQL = `snippet(search_conversation_fts, 1, char(2), char(3), '…',
	CASE WHEN instr(search_conversation_fts.body, char(31)) THEN 64 ELSE 24 END)`

func bodySnippet(snippet string) []Segment {
	if !strings.ContainsRune(snippet, markStart) {
		return []Segment{}
	}
	return segments(snippet)
}

const ownConversationSQL = `c.workspace_id = ? AND c.owner_id = ?
	AND EXISTS (SELECT 1 FROM memberships m WHERE m.workspace_id = c.workspace_id AND m.user_id = c.owner_id)
	AND EXISTS (SELECT 1 FROM users u WHERE u.id = c.owner_id AND u.disabled_at IS NULL)`

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
	allowance := x.conversationSyncLimit
	syncCtx, stopSync := context.WithTimeout(ctx, conversationSyncBudget)
	defer stopSync()
	for try := 1; ; try++ {
		if allowance > 0 && syncCtx.Err() == nil {
			n, err := x.syncConversations(syncCtx, src, userID, workspaceID, allowance)
			allowance -= n
			switch {
			case ctx.Err() != nil:
				return ConversationResults{}, ctx.Err()
			case err != nil && syncCtx.Err() != nil:
			case err != nil:
				x.log.Warn("search: indexing conversations before a search; its results may be behind", "err", err)
			}
		}
		var behind bool
		var err error
		if found, behind, err = x.findConversations(ctx, userID, workspaceID, match, q); err != nil {
			return ConversationResults{}, err
		}
		if !behind {
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
	if next := q.Offset + q.Limit; more && !res.Incomplete && next <= MaxOffset {
		res.NextOffset = &next
	}
	return res, nil
}

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
			UpdatedAt: updatedAt.String, Snippet: bodySnippet(snippet.String)})
	}
	return found, behind, rows.Err()
}

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

const (
	conversationPassMax    = 200
	conversationPassBudget = 2 * time.Minute
)

func (x *Index) conversationLoop(ctx context.Context) {
	tick := time.NewTicker(sweepEvery)
	defer tick.Stop()
	for {
		if x.syncConversationsInBackground(ctx) {
			wake(x.convWake)
		}
		select {
		case <-ctx.Done():
			return
		case <-x.convWake:
		case <-tick.C:
		}
	}
}

func (x *Index) syncConversationsInBackground(ctx context.Context) (more bool) {
	src := x.conversationSource()
	if src == nil {
		return false
	}
	passCtx, cancel := context.WithTimeout(ctx, conversationPassBudget)
	defer cancel()
	n, err := x.syncConversations(passCtx, src, 0, 0, x.conversationPassLimit)
	if err != nil && ctx.Err() == nil {
		if errors.Is(err, conversations.ErrNoTranscript) {
			x.log.Debug("search: the agent runtime can't give conversations' transcripts now", "err", err)
		} else {
			x.log.Error("search: indexing conversations", "err", err)
		}
		return false
	}
	if ctx.Err() != nil || n == 0 {
		return false
	}
	var left bool
	if err := x.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM search_conversation_queue)`).Scan(&left); err != nil {
		return false
	}
	return left
}

type queuedConversation struct {
	id, agentID, revision int64
}

func (x *Index) syncConversations(ctx context.Context, src ConversationSource, userID, workspaceID int64, limit int) (read int, err error) {
	everyone := userID == 0
	skip := []int64{}
	var firstErr error
	for done := 0; ; {
		if limit > 0 && done >= limit {
			return done, firstErr
		}
		skipJSON, err := json.Marshal(skip)
		if err != nil {
			return done, err
		}
		batch, err := x.queuedConversations(ctx, everyone, userID, workspaceID, string(skipJSON))
		if err != nil {
			return done, err
		}
		if len(batch) == 0 {
			return done, firstErr
		}
		for _, qc := range batch {
			if limit > 0 && done >= limit {
				return done, firstErr
			}
			done++
			text, err := src.Transcript(ctx, qc.agentID)
			gone := errors.Is(err, conversations.ErrGone)
			if err != nil && !gone {
				if ctx.Err() != nil {
					return done, firstErr
				}
				if firstErr == nil {
					firstErr = err
				}
				if errors.Is(err, conversations.ErrRuntimeDown) {
					return done, firstErr
				}
				skip = append(skip, qc.id)
				continue
			}
			if err := x.write(ctx, func(tx *sql.Tx) error { return indexConversation(ctx, tx, qc, text, gone) }); err != nil {
				return done, err
			}
		}
		still, err := x.stillQueued(ctx, batch)
		if err != nil {
			return done, err
		}
		skip = append(skip, still...)
	}
}

func (x *Index) stillQueued(ctx context.Context, batch []queuedConversation) ([]int64, error) {
	ids := make([]int64, len(batch))
	for i, qc := range batch {
		ids[i] = qc.id
	}
	in, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := x.db.QueryContext(ctx, `SELECT conversation_id FROM search_conversation_queue
		WHERE conversation_id IN (SELECT value FROM json_each(?))`, string(in))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

const (
	queuedConversationsHeadSQL = `SELECT sq.conversation_id, c.agent_id, rv.revision
		FROM search_conversation_queue sq
		JOIN conversations c ON c.id = sq.conversation_id
		JOIN search_conversation_revisions rv ON rv.conversation_id = sq.conversation_id
		WHERE `
	queuedConversationsTailSQL = ` AND sq.conversation_id NOT IN (SELECT value FROM json_each(?))
		ORDER BY c.updated_at DESC, c.id DESC LIMIT ?`
	queuedEveryoneSQL = queuedConversationsHeadSQL + `1` + queuedConversationsTailSQL
	queuedOwnSQL      = queuedConversationsHeadSQL + ownConversationSQL + queuedConversationsTailSQL
)

func (x *Index) queuedConversations(ctx context.Context, everyone bool, userID, workspaceID int64, skip string) ([]queuedConversation, error) {
	query, args := queuedEveryoneSQL, []any{}
	if !everyone {
		query, args = queuedOwnSQL, []any{workspaceID, userID}
	}
	rows, err := x.db.QueryContext(ctx, query, append(args, skip, conversationBatch)...)
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

func indexConversation(ctx context.Context, tx *sql.Tx, qc queuedConversation, text string, gone bool) error {
	var title string
	var revision, agentID int64
	err := tx.QueryRowContext(ctx, `SELECT c.title, c.agent_id, rv.revision FROM conversations c
		JOIN search_conversation_revisions rv ON rv.conversation_id = c.id WHERE c.id = ?`, qc.id).Scan(&title, &agentID, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if agentID != qc.agentID || revision != qc.revision {
		return nil
	}
	if !gone {
		var queued int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM search_conversation_queue WHERE conversation_id = ?`, qc.id).Scan(&queued); err != nil {
			return err
		}
		if queued == 0 {
			return nil
		}
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
