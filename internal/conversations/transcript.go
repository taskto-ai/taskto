package conversations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"taskto.ai/taskto/internal/agentrt"
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

func (s *Service) markWhenSettled(rt Runtime, id, agentID, sub int64) {
	go func() {
		ctx, cancel := context.WithTimeout(s.closing, settleWait)
		defer cancel()
		if _, err := rt.Wait(ctx, sub); err != nil {
			return
		}
		if err := s.touch(s.closing, id, agentID); err != nil && s.closing.Err() == nil {
			slog.Warn("conversations: marking a conversation updated after a reply", "conversation", id, "err", err)
		}
	}()
}
