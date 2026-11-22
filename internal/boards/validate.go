package boards

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

// Limits on what a card holds.
const (
	maxNameLen   = 100 // board names, in characters
	maxTitleLen  = 200 // card titles, in characters
	maxBodyBytes = 32 << 10
	maxReasonLen = 1000 // blocked reasons, in characters
	maxLinks     = 50   // of each kind, and blockers
	maxPathLen   = 1024 // doc paths, in bytes
)

// Optional is a field that can be left out, set to null, or set to a value, for the
// fields of an update where null means "clear it".
type Optional[T any] struct {
	// Set is true when the field was given, null included.
	Set bool
	// Value is nil for null.
	Value *T
}

// UnmarshalJSON records that the field was given.
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	o.Set = true
	o.Value = nil
	if string(data) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(&o.Value)
}

func boardName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n < 1 || n > maxNameLen {
		return "", invalidAs(apierr.CodeBoardNameLength, apierr.Params{"max": maxNameLen},
			fmt.Sprintf("a board name of 1 to %d characters is required", maxNameLen))
	}
	return s, nil
}

func commentBody(s string) (string, error) {
	if strings.TrimSpace(s) == "" || len(s) > maxBodyBytes || !utf8.ValidString(s) {
		return "", invalidAs(apierr.CodeCommentLength, apierr.Params{"max": maxBodyBytes >> 10},
			fmt.Sprintf("a comment of 1 byte to %d KiB of UTF-8 is required", maxBodyBytes>>10))
	}
	return s, nil
}

// validate checks and normalizes the fields ch sets. An assignee must be the agent or a
// member of the workspace who may read sp; whether blockers are on the board is checked
// when they are written.
func (s *Service) validate(ctx context.Context, sp access.Space, ch *CardChanges) error {
	if ch.Title != nil {
		t := strings.TrimSpace(*ch.Title)
		if n := utf8.RuneCountInString(t); n < 1 || n > maxTitleLen || hasControl(t) {
			return invalidAs(apierr.CodeCardTitleInvalid, apierr.Params{"max": maxTitleLen},
				fmt.Sprintf("a title of 1 to %d characters, on one line, is required", maxTitleLen))
		}
		ch.Title = &t
	}
	if ch.Body != nil && (len(*ch.Body) > maxBodyBytes || !utf8.ValidString(*ch.Body)) {
		return invalidAs(apierr.CodeCardBodyTooLong, apierr.Params{"max": maxBodyBytes >> 10},
			fmt.Sprintf("the body must be UTF-8 of at most %d KiB", maxBodyBytes>>10))
	}
	if ch.BlockedReason.Value != nil {
		r := strings.TrimSpace(*ch.BlockedReason.Value)
		if n := utf8.RuneCountInString(r); n < 1 || n > maxReasonLen {
			return invalidAs(apierr.CodeBlockedReasonLength, apierr.Params{"max": maxReasonLen},
				fmt.Sprintf("a blocked reason has 1 to %d characters; null clears it", maxReasonLen))
		}
		ch.BlockedReason.Value = &r
	}
	if ch.BlockedBy != nil {
		ids := dedupe(*ch.BlockedBy)
		if len(ids) > maxLinks {
			return invalidAs(apierr.CodeTooManyLinks, apierr.Params{"max": maxLinks}, fmt.Sprintf("a card waits for at most %d cards", maxLinks))
		}
		ch.BlockedBy = &ids
	}
	if ch.Docs != nil {
		docs := dedupe(*ch.Docs)
		if len(docs) > maxLinks {
			return invalidAs(apierr.CodeTooManyLinks, apierr.Params{"max": maxLinks}, fmt.Sprintf("a card links to at most %d docs", maxLinks))
		}
		for _, d := range docs {
			if len(d) > maxPathLen || access.CheckName(d) != nil {
				return invalidAs(apierr.CodeDocPathInvalid, apierr.Params{"path": d},
					fmt.Sprintf("docs: %q isn't a path within the space, like notes/plan.md", d))
			}
		}
		ch.Docs = &docs
	}
	if ch.Conversations != nil {
		convs := dedupe(*ch.Conversations)
		if len(convs) > maxLinks {
			return invalidAs(apierr.CodeTooManyLinks, apierr.Params{"max": maxLinks}, fmt.Sprintf("a card links to at most %d conversations", maxLinks))
		}
		for _, c := range convs {
			if _, ok := conversationID(c); !ok {
				return invalid(fmt.Sprintf("conversations: %q isn't a conversation ID", c))
			}
		}
		ch.Conversations = &convs
	}
	if as := ch.Assignee.Value; as != nil {
		switch as.Kind {
		case AssignAgent:
			*as = Assignee{Kind: AssignAgent}
		case AssignUser:
			ok, err := s.access.CanRead(ctx, as.ID, sp.ID)
			if err != nil {
				return err
			}
			if !ok {
				return invalidAs(apierr.CodeAssigneeNoAccess, nil, "the assignee must be a member of the workspace who can see this space")
			}
			*as = Assignee{Kind: AssignUser, ID: as.ID}
		default:
			return invalid(`assignee: kind must be "user" or "agent"; null unassigns`)
		}
	}
	return nil
}

// conversationID parses a conversation link: the conversation API's ID, in decimal without
// a sign or leading zeros, so each conversation has one spelling.
func conversationID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0 && strconv.FormatInt(id, 10) == s
}

func hasControl(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }

// dedupe drops repeats, keeping the first of each. The result is never nil.
func dedupe[T comparable](in []T) []T {
	out := make([]T, 0, len(in))
	for _, v := range in {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
