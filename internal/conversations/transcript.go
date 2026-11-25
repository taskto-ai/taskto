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

// ErrGone is Transcript's answer for a conversation the agent runtime doesn't have (any more):
// it was never there, was retired, or its data was lost (see conversationError).
var ErrGone = errors.New("the agent runtime doesn't have this conversation")

// settleWait is how long a message's turn is waited for before the conversation is marked
// updated anyway: a turn that runs longer is found by the next message's, or by a restart.
const settleWait = time.Hour

// Transcript returns what was said in a conversation, as the agent runtime's transcript has it:
// the text of its user and assistant messages, oldest first, each ending with a line break.
// Not the system's own instructions, and not what tools returned: what people and the agent
// wrote to each other. agentID is the agent runtime's ID of the conversation (the search index
// reads it from the conversations table, next to the owner it looks for): nothing here checks
// who owns it, so a caller passes only the IDs of conversations it has checked.
//
// It returns ErrNoTranscript if the agent runtime can't give it now (off, starting, no answer
// within the time it is given), and ErrGone if the agent runtime doesn't have the conversation.
func (s *Service) Transcript(ctx context.Context, agentID int64) (string, error) {
	if s.agent == nil {
		return "", fmt.Errorf("%w: the agent runtime is off", ErrNoTranscript)
	}
	rt, err := s.exportRuntime(ctx)
	if err != nil {
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
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			return "", ErrGone
		}
		return "", fmt.Errorf("%w: %w", ErrNoTranscript, err)
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

// markWhenSettled marks conversation id updated again once the agent runtime has dealt with
// submission sub, which is when the agent's reply to it is in the transcript: the send marked it
// updated when the message was admitted, and that is all the server sees of what the agent
// says after. Whatever follows the conversation's updates (the search index, which indexes the
// transcript when a conversation is marked) then catches the reply too.
func (s *Service) markWhenSettled(rt Runtime, id, sub int64) {
	go func() {
		ctx, cancel := context.WithTimeout(s.closing, settleWait)
		defer cancel()
		if _, err := rt.Wait(ctx, sub); err != nil && ctx.Err() != nil {
			return // the server is shutting down, or the turn runs on: its next mark catches up
		}
		if err := s.touch(s.closing, id); err != nil && s.closing.Err() == nil {
			slog.Warn("conversations: marking a conversation updated after a reply", "conversation", id, "err", err)
		}
	}()
}
