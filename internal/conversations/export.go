package conversations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNoTranscript is the error of ExportEntries when the agent runtime can't give a
// conversation's history now: it is off, still starting, or no longer has the conversation.
var ErrNoTranscript = errors.New("the conversation's history isn't available")

// exportPage is how many entries ExportEntries asks the agent runtime for at once.
const exportPage = maxPageEntries

// ExportList returns every conversation user owns in workspace, archived ones too, oldest
// first, if they are still a member there: the data export's list (docs/api.md#export-my-data).
// Nobody else's conversation is in it.
func (s *Service) ExportList(ctx context.Context, workspace, user int64) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM conversations c
		WHERE workspace_id = ? AND owner_id = ?
		AND EXISTS (SELECT 1 FROM memberships m WHERE m.workspace_id = c.workspace_id AND m.user_id = c.owner_id)
		ORDER BY created_at, id`, workspace, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Conversation{}
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ExportEntries calls emit with each entry of c's history, oldest first, as the agent runtime
// sends them (what the web app draws). c must come from ExportList: nothing here checks who
// owns it. It returns ErrNoTranscript if the agent runtime can't give the history. A page of
// the history is held at a time, and the pages are read newest first, so the whole history is
// held before the first entry is emitted.
func (s *Service) ExportEntries(ctx context.Context, c Conversation, emit func(json.RawMessage) error) error {
	if s.agent == nil {
		return fmt.Errorf("%w: the agent runtime is off", ErrNoTranscript)
	}
	rt, err := s.agent(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %w", ErrNoTranscript, err)
	}
	var pages [][]json.RawMessage
	var before int64
	for {
		raw, err := rt.Entries(ctx, c.agentID, before, exportPage)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w: %w", ErrNoTranscript, err)
		}
		var page struct {
			Entries []json.RawMessage `json:"entries"`
			// Before is there if the page doesn't start the history.
			Before *int64 `json:"before"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("%w: %w", ErrNoTranscript, err)
		}
		if len(page.Entries) == 0 {
			break
		}
		pages = append(pages, page.Entries)
		if page.Before == nil {
			break
		}
		// The next page is the one before this page's first entry.
		var first struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(page.Entries[0], &first); err != nil || first.ID < 1 || (before != 0 && first.ID >= before) {
			return fmt.Errorf("%w: entries out of order", ErrNoTranscript)
		}
		before = first.ID
	}
	for i := len(pages) - 1; i >= 0; i-- {
		for _, e := range pages[i] {
			if err := emit(e); err != nil {
				return err
			}
		}
	}
	return nil
}
