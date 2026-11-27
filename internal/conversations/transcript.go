package conversations

import (
	"context"
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

func (s *Service) markWhenSettled(rt Runtime, p Principal, engine string, cursor, sub int64) {
	go func() {
		ctx, cancel := context.WithTimeout(s.closing, settleWait)
		defer cancel()
		if _, err := rt.Wait(ctx, sub); err != nil {
			return
		}
		if cursor >= 0 {
			if err := s.recordAgentUsage(ctx, rt, p, engine, cursor); err != nil {
				slog.Warn("conversations: recording agent token usage", "conversation", p.Conversation, "err", err)
			}
		}
		if err := s.touch(s.closing, p.Conversation, p.AgentConversation); err != nil && s.closing.Err() == nil {
			slog.Warn("conversations: marking a conversation updated after a reply", "conversation", p.Conversation, "err", err)
		}
	}()
}

func (s *Service) recordAgentUsage(ctx context.Context, rt Runtime, p Principal, engine string, cursor int64) error {
	type totals struct{ input, output int64 }
	totalsByEngine := map[string]totals{}
	before := int64(0)
	for pages := 0; pages < 1000; pages++ {
		raw, err := rt.Entries(ctx, p.AgentConversation, before, maxPageEntries)
		if err != nil {
			return err
		}
		var page entryPage
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
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
				return err
			}
			if oldest == 0 || entry.ID < oldest {
				oldest = entry.ID
			}
			if entry.ID <= cursor {
				continue
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
		if oldest == 0 || oldest <= cursor || page.Before == nil || *page.Before <= 0 {
			break
		}
		before = *page.Before
	}
	for name, count := range totalsByEngine {
		if err := usage.Record(ctx, s.db, usage.Entry{User: p.User, Workspace: p.Workspace, Source: "agent", Engine: name, Input: count.input, Output: count.output, At: time.Now()}); err != nil {
			return err
		}
	}
	return nil
}
