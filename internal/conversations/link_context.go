package conversations

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"
)

const (
	skipUnavailable = "unavailable"
	skipTooLarge    = "too_large"

	maxLinkedBytes      = 24 << 10
	maxLinkedTotalBytes = 48 << 10
	minLinkedBytes      = 1 << 10
	maxEntryData        = 4096
)

type LinkSkip struct {
	Conversation int64  `json:"conversation"`
	Reason       string `json:"reason"`
}

type attachedConversation struct {
	Conversation int64  `json:"conversation"`
	Title        string `json:"title"`

	text string
}

type inputData struct {
	V        int                    `json:"v"`
	Author   int64                  `json:"author"`
	Text     string                 `json:"text,omitempty"`
	Attached []attachedConversation `json:"attached,omitempty"`
}

func tail(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := len(text) - limit
	kept := text[cut:]
	if text[cut-1] == '\n' {
		return kept
	}
	if i := strings.IndexByte(kept, '\n'); i >= 0 && i+1 < len(kept) {
		return kept[i+1:]
	}
	for len(kept) > 0 && !utf8.RuneStart(kept[0]) {
		kept = kept[1:]
	}
	return kept
}

func (s *Service) readLinked(ctx context.Context, workspace, user, id int64) (attachedConversation, bool) {
	c, err := s.find(ctx, workspace, user, id)
	if err != nil {
		return attachedConversation{}, false
	}
	waitCtx, cancel := context.WithTimeout(ctx, s.linkWait)
	defer cancel()
	text, err := s.Transcript(waitCtx, c.agentID)
	if err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "conversations: a linked conversation's transcript could not be read", "conversation", id, "err", err)
	}
	if strings.TrimSpace(text) == "" {
		return attachedConversation{}, false
	}
	again, err := s.find(ctx, workspace, user, id)
	if err != nil || again.agentID != c.agentID {
		return attachedConversation{}, false
	}
	return attachedConversation{Conversation: id, Title: again.Title, text: text}, true
}

func (s *Service) attach(ctx context.Context, workspace, user, current int64, content string) ([]attachedConversation, []LinkSkip) {
	var attached []attachedConversation
	var skipped []LinkSkip
	budget := maxLinkedTotalBytes
	for _, id := range s.linkedIn(content, workspace, current) {
		a, ok := s.readLinked(ctx, workspace, user, id)
		if !ok {
			skipped = append(skipped, LinkSkip{Conversation: id, Reason: skipUnavailable})
			continue
		}
		if budget < minLinkedBytes {
			skipped = append(skipped, LinkSkip{Conversation: id, Reason: skipTooLarge})
			continue
		}
		a.text = tail(a.text, min(maxLinkedBytes, budget))
		budget -= len(a.text)
		attached = append(attached, a)
	}
	return attached, skipped
}

func withContext(typed string, attached []attachedConversation) string {
	var b strings.Builder
	b.WriteString(typed)
	for _, a := range attached {
		title := strings.NewReplacer(`"`, `'`, "\n", " ", "\r", " ").Replace(a.Title)
		fmt.Fprintf(&b, "\n\n---\nContext from the conversation \"%s\" (%d), as the person attached it:\n%s", title, a.Conversation, strings.TrimRight(a.text, "\n"))
	}
	return b.String()
}

func (s *Service) messageWithLinks(ctx context.Context, workspace, user, current int64, typed string) (string, json.RawMessage, []LinkSkip, error) {
	attached, skipped := s.attach(ctx, workspace, user, current, typed)
	data, err := json.Marshal(inputData{V: 1, Author: user})
	if err != nil || len(attached) == 0 {
		return typed, data, skipped, err
	}
	withText, err := json.Marshal(inputData{V: 1, Author: user, Text: typed, Attached: attached})
	if err != nil {
		return "", nil, nil, err
	}
	if len(withText) > maxEntryData {
		for _, a := range attached {
			skipped = append(skipped, LinkSkip{Conversation: a.Conversation, Reason: skipTooLarge})
		}
		return typed, data, skipped, nil
	}
	return withContext(typed, attached), withText, skipped, nil
}
