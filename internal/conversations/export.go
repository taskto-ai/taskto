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
// owns it. It returns ErrNoTranscript if the agent runtime can't give the history, or doesn't
// answer within the time it is given (exportTimeout for each page, readyWait for it to be ready),
// so a stalled agent runtime can't hold an export for ever.
//
// The runtime pages back from the newest entry, so the history is read twice, a page at a time:
// first back to the oldest entry, keeping only where each page starts, then forward, emitting
// each page. A page of the history is held at a time, however long it is. The history only grows,
// and the newest entry is fixed when the first read ends, so entries added meanwhile are left for
// the next export, not repeated or half read.
func (s *Service) ExportEntries(ctx context.Context, c Conversation, emit func(json.RawMessage) error) error {
	if s.agent == nil {
		return fmt.Errorf("%w: the agent runtime is off", ErrNoTranscript)
	}
	rt, err := s.exportRuntime(ctx)
	if err != nil {
		return err
	}
	// cursors[i] is the "before" of the i-th page from the newest: entries with a smaller ID.
	var cursors []int64
	var before int64
	for {
		entries, more, err := s.exportPageOf(ctx, rt, c, before)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			break
		}
		first, err := entryID(entries[0])
		if err != nil || first < 1 || (before != 0 && first >= before) {
			return fmt.Errorf("%w: entries out of order", ErrNoTranscript)
		}
		if before == 0 {
			// Pin the newest entry: the newest page is read again from just past it.
			last, err := entryID(entries[len(entries)-1])
			if err != nil || last < first {
				return fmt.Errorf("%w: entries out of order", ErrNoTranscript)
			}
			cursors = append(cursors, last+1)
		} else {
			cursors = append(cursors, before)
		}
		if !more {
			break
		}
		before = first // the next page is the one before this page's first entry
	}
	for i := len(cursors) - 1; i >= 0; i-- {
		entries, _, err := s.exportPageOf(ctx, rt, c, cursors[i])
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := emit(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// exportRuntime returns the agent runtime, waiting up to readyWait for one that is starting.
func (s *Service) exportRuntime(ctx context.Context) (Runtime, error) {
	wait, cancel := context.WithTimeout(ctx, s.readyWait)
	defer cancel()
	rt, err := s.agent(wait)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", ErrNoTranscript, err)
	}
	return rt, nil
}

// exportPageOf reads the page of c's history before the entry ID before (0: the newest), within
// exportTimeout. more says the history goes on before it.
func (s *Service) exportPageOf(ctx context.Context, rt Runtime, c Conversation, before int64) (entries []json.RawMessage, more bool, err error) {
	pageCtx, cancel := context.WithTimeout(ctx, s.exportTimeout)
	defer cancel()
	raw, err := rt.Entries(pageCtx, c.agentID, before, exportPage)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, fmt.Errorf("%w: %w", ErrNoTranscript, err)
	}
	var page struct {
		Entries []json.RawMessage `json:"entries"`
		// Before is there if the page doesn't start the history.
		Before *int64 `json:"before"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrNoTranscript, err)
	}
	return page.Entries, page.Before != nil, nil
}

func entryID(e json.RawMessage) (int64, error) {
	var v struct {
		ID int64 `json:"id"`
	}
	err := json.Unmarshal(e, &v)
	return v.ID, err
}
