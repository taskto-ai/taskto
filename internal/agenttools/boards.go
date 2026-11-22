package agenttools

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"taskto.ai/taskto/internal/boards"
)

// Bounds of the board tools: a card's body or a comment can be 32 KiB, so lists and
// threads cut them, and say so.
const (
	listBodyLen    = 500       // a card's body in list_cards, in bytes
	threadBodyLen  = 16 << 10  // a card's body in show_card
	commentLen     = 2 << 10   // each comment in show_card
	maxTextFilter  = 200       // list_cards' text, in characters
	truncatedNotes = " […cut]" // marks a cut text
)

// ref names a board, or a card of one, in a space.
type ref struct {
	SpaceID int64 `json:"space_id"`
	BoardID int64 `json:"board_id"`
}

type cardRef struct {
	ref
	CardID int64 `json:"card_id"`
}

// actor is the call's user, as the agent working for them, marked with the access token
// the call came with, if any.
func actor(c *call) boards.Actor { return boards.Actor{UserID: c.User, Agent: true, Via: c.Token} }

// boardSpace checks the call's user may read spaceID, in the call's workspace.
// The boards' methods check the user's access again themselves.
func (s *Service) boardSpace(ctx context.Context, c *call, spaceID int64) error {
	_, err := s.space(ctx, c, spaceID)
	return err
}

func (s *Service) listBoards(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID int64 `json:"space_id"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.boardSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	list, err := s.o.Boards.ListBoards(ctx, actor(c), a.SpaceID)
	if err != nil {
		return nil, err
	}
	n, res, err := fitting(len(list), func(n int) any { return map[string]any{"boards": list[:n]} })
	if err == nil && n < len(list) {
		res = map[string]any{"boards": list[:n], "truncated": true}
	}
	return res, err
}

func (s *Service) listCards(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		ref
		Column     boards.Column       `json:"column"`
		Assignee   boards.AssigneeKind `json:"assignee"`
		AssigneeID int64               `json:"assignee_id"`
		Blocked    *bool               `json:"blocked"`
		Text       string              `json:"text"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	switch a.Assignee {
	case "", boards.AssignAgent, boards.Unassigned:
	case boards.AssignUser:
		if a.AssigneeID == 0 {
			return nil, errInvalid(`assignee "user" needs assignee_id`)
		}
	default:
		return nil, errInvalid(`assignee must be "user" (with assignee_id), "agent" or "none"`)
	}
	if utf8.RuneCountInString(a.Text) > maxTextFilter {
		return nil, invalidf("text is at most %d characters", maxTextFilter)
	}
	if err := s.boardSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	cards, err := s.o.Boards.ListCards(ctx, actor(c), a.SpaceID, a.BoardID, boards.Filter{
		Column: a.Column, Assignee: a.Assignee, AssigneeID: a.AssigneeID, Blocked: a.Blocked, Text: a.Text,
	})
	if err != nil {
		return nil, err
	}
	for i := range cards {
		cards[i].Body = cutText(cards[i].Body, listBodyLen)
	}
	n, res, err := fitting(len(cards), func(n int) any { return map[string]any{"cards": cards[:n]} })
	if err == nil && n < len(cards) {
		res = map[string]any{"cards": cards[:n], "truncated": true,
			"notice": "more cards than fit: narrow the list with column, assignee, blocked or text"}
	}
	return res, err
}

func (s *Service) showCard(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a cardRef
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.boardSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	t, err := s.o.Boards.ShowCard(ctx, actor(c), a.SpaceID, a.BoardID, a.CardID)
	if err != nil {
		return nil, err
	}
	t.Body = cutText(t.Body, threadBodyLen)
	for i := range t.Comments {
		t.Comments[i].Body = cutText(t.Comments[i].Body, commentLen)
	}
	// The newest comments that fit.
	all := t.Comments
	_, res, err := fitting(len(all), func(n int) any {
		shown := t
		shown.Comments = all[len(all)-n:]
		if n == len(all) {
			return shown
		}
		return struct {
			boards.Thread
			Omitted int `json:"older_comments_omitted"`
		}{shown, len(all) - n}
	})
	return res, err
}

func (s *Service) createCard(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		ref
		boards.NewCard
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.boardSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	return s.o.Boards.CreateCard(ctx, actor(c), a.SpaceID, a.BoardID, a.NewCard)
}

func (s *Service) updateCard(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		cardRef
		boards.CardChanges
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.boardSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	return s.o.Boards.UpdateCard(ctx, actor(c), a.SpaceID, a.BoardID, a.CardID, a.CardChanges)
}

func (s *Service) moveCard(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		cardRef
		boards.Move
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.boardSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	return s.o.Boards.MoveCard(ctx, actor(c), a.SpaceID, a.BoardID, a.CardID, a.Move)
}

func (s *Service) commentCard(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		cardRef
		Body string `json:"body"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.boardSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	return s.o.Boards.Comment(ctx, actor(c), a.SpaceID, a.BoardID, a.CardID, a.Body)
}

func (s *Service) setBlocked(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		cardRef
		Version   int64   `json:"version"`
		Reason    *string `json:"reason"`
		BlockedBy []int64 `json:"blocked_by"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.boardSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	return s.o.Boards.SetBlocked(ctx, actor(c), a.SpaceID, a.BoardID, a.CardID, a.Version, a.Reason, a.BlockedBy)
}

// cutText shortens s to n bytes, marking the cut.
func cutText(s string, n int) string {
	if short, cut := cut(s, n); cut {
		return short + truncatedNotes
	}
	return s
}
