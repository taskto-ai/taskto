package conversations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/usage"
)

var ErrGone = errors.New("the agent runtime doesn't have this conversation")

var ErrRuntimeDown = errors.New("the agent runtime isn't answering")

const settleWait = time.Hour

func (s *Service) Transcript(ctx context.Context, agentID int64) (string, error) {
	if s.agent == nil {
		return "", fmt.Errorf("%w: %w: the agent runtime is off", ErrNoTranscript, ErrRuntimeDown)
	}
	rt, err := s.exportRuntime(ctx)
	if err != nil {
		if errors.Is(err, ErrNoTranscript) {
			err = fmt.Errorf("%w: %w", ErrRuntimeDown, err)
		}
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, s.exportTimeout)
	defer cancel()
	msgs, err := rt.Messages(callCtx, agentID)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var se *agentrt.StatusError
		switch {
		case errors.As(err, &se) && se.Code == http.StatusNotFound:
			return "", ErrGone
		case errors.As(err, &se):
			return "", fmt.Errorf("%w: %w", ErrNoTranscript, err)
		}
		return "", fmt.Errorf("%w: %w: %w", ErrNoTranscript, ErrRuntimeDown, err)
	}
	var b strings.Builder
	for _, m := range msgs {
		if (m.Role != "user" && m.Role != "assistant") || strings.TrimSpace(m.Text) == "" {
			continue
		}
		b.WriteString(m.Text)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

type entryPage struct {
	Entries []json.RawMessage `json:"entries"`
	Before  *int64            `json:"before"`
}

func (s *Service) entryCursor(ctx context.Context, rt Runtime, agentID int64) int64 {
	raw, err := rt.Entries(ctx, agentID, 0, maxPageEntries)
	if err != nil {
		return -1
	}
	var page entryPage
	if json.Unmarshal(raw, &page) != nil {
		return -1
	}
	var cursor int64
	for _, raw := range page.Entries {
		var entry struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			return -1
		}
		if entry.ID > cursor {
			cursor = entry.ID
		}
	}
	return cursor
}

func (s *Service) markWhenSettled(rt Runtime, p Principal, engine string, sub int64) {
	go func() {
		ctx, cancel := context.WithTimeout(s.closing, settleWait)
		defer cancel()
		if _, err := rt.Wait(ctx, sub); err != nil {
			return
		}
		if err := s.recordAgentUsage(ctx, rt, p, engine); err != nil {
			slog.Warn("conversations: recording agent token usage", "conversation", p.Conversation, "err", err)
		}
		if err := s.touch(s.closing, p.Conversation, p.AgentConversation); err != nil && s.closing.Err() == nil {
			slog.Warn("conversations: marking a conversation updated after a reply", "conversation", p.Conversation, "err", err)
		}
	}()
}

type usageTotals struct{ input, output int64 }

func (s *Service) startUsageMark(ctx context.Context, conversation, cursor int64) {
	if cursor < 0 {
		return
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE conversations SET usage_mark = ? WHERE id = ? AND usage_mark IS NULL`, cursor, conversation); err != nil {
		slog.WarnContext(ctx, "conversations: starting agent usage counting", "conversation", conversation, "err", err)
	}
}

func (s *Service) recordAgentUsage(ctx context.Context, rt Runtime, p Principal, engine string) error {
	for attempt := 0; attempt < 5; attempt++ {
		var mark sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT usage_mark FROM conversations WHERE id = ?`, p.Conversation).Scan(&mark); err != nil {
			return err
		}
		if !mark.Valid {
			return nil
		}
		totals, top, err := s.collectAgentUsage(ctx, rt, p.AgentConversation, engine, mark.Int64)
		if err != nil {
			return err
		}
		if top <= mark.Int64 {
			return nil
		}
		claimed, err := s.claimAgentUsage(ctx, p, mark.Int64, top, totals)
		if err != nil || claimed {
			return err
		}
	}
	return nil
}

func (s *Service) claimAgentUsage(ctx context.Context, p Principal, mark, top int64, totals map[string]usageTotals) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE conversations SET usage_mark = ? WHERE id = ? AND usage_mark = ?`, top, p.Conversation, mark)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	for name, count := range totals {
		if err := usage.RecordTx(ctx, tx, usage.Entry{User: p.User, Workspace: p.Workspace, Source: "agent", Engine: name, Input: count.input, Output: count.output, At: time.Now()}); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (s *Service) collectAgentUsage(ctx context.Context, rt Runtime, agentID int64, engine string, mark int64) (map[string]usageTotals, int64, error) {
	totalsByEngine := map[string]usageTotals{}
	top, before := int64(0), int64(0)
	for pages := 0; pages < 1000; pages++ {
		raw, err := rt.Entries(ctx, agentID, before, maxPageEntries)
		if err != nil {
			return nil, 0, err
		}
		var page entryPage
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, 0, err
		}
		oldest := int64(0)
		for _, rawEntry := range page.Entries {
			var entry struct {
				ID    int64 `json:"id"`
				Model []struct {
					Role     string `json:"role"`
					Provider string `json:"provider"`
					Usage    *struct {
						Input  int64 `json:"input"`
						Output int64 `json:"output"`
					} `json:"usage"`
				} `json:"model"`
			}
			if err := json.Unmarshal(rawEntry, &entry); err != nil {
				return nil, 0, err
			}
			if oldest == 0 || entry.ID < oldest {
				oldest = entry.ID
			}
			if entry.ID <= mark {
				continue
			}
			if entry.ID > top {
				top = entry.ID
			}
			for _, message := range entry.Model {
				if message.Role != "assistant" || message.Usage == nil {
					continue
				}
				name := message.Provider
				if name == "" {
					name = engine
				}
				if name == "" || len(name) > 64 || message.Usage.Input < 0 || message.Usage.Output < 0 {
					continue
				}
				current := totalsByEngine[name]
				current.input += message.Usage.Input
				current.output += message.Usage.Output
				totalsByEngine[name] = current
			}
		}
		if oldest == 0 || oldest <= mark || page.Before == nil || *page.Before <= 0 {
			break
		}
		before = oldest
	}
	return totalsByEngine, top, nil
}
