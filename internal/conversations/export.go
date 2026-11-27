package conversations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrNoTranscript = errors.New("the conversation's history isn't available")

const exportPage = maxPageEntries

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

func (s *Service) ExportEntries(ctx context.Context, c Conversation, emit func(json.RawMessage) error) error {
	if s.agent == nil {
		return fmt.Errorf("%w: the agent runtime is off", ErrNoTranscript)
	}
	rt, err := s.exportRuntime(ctx)
	if err != nil {
		return err
	}
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
		before = first
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
		Before  *int64            `json:"before"`
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
