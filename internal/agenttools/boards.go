package agenttools

import (
	"context"
	"encoding/json"
	"slices"
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
		SpaceID  int64 `json:"space_id"`
		Archived bool  `json:"archived"`
		After    int64 `json:"after"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.After < 0 {
		return nil, errInvalid("after must be a board id from a previous next")
	}
	if err := s.boardSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	var list []boards.Board
	var err error
	if a.Archived {
		list, err = s.o.Boards.ListAllBoards(ctx, actor(c), a.SpaceID)
	} else {
		list, err = s.o.Boards.ListBoards(ctx, actor(c), a.SpaceID)
	}
	if err != nil {
		return nil, err
	}
	if a.After != 0 {
		// The page after the board `after`, in the list's order.
		i := slices.IndexFunc(list, func(b boards.Board) bool { return b.ID == a.After })
		if i < 0 {
			return nil, errInvalid("after is not a board of this list: use the next of the previous page, with the same archived")
		}
		list = list[i+1:]
	}
	// truncated and next are part of what is measured, so the page fits with them.
	_, res, err := fitting(len(list), func(n int) any {
		if n == len(list) {
			return map[string]any{"boards": list[:n]}
		}
		next := a.After // nothing fit: no progress, which a board's size makes impossible
		if n > 0 {
			next = list[n-1].ID
		}
		return map[string]any{"boards": list[:n], "truncated": true, "next": next}
	})
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
		Labels     []string            `json:"labels"`
		DueBefore  string              `json:"due_before"`
		DueAfter   string              `json:"due_after"`
		Overdue    bool                `json:"overdue"`
		Today      string              `json:"today"`
		Sort       boards.Sort         `json:"sort"`
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
		Labels: a.Labels, DueBefore: a.DueBefore, DueAfter: a.DueAfter, Overdue: a.Overdue, Today: a.Today, Sort: a.Sort,
	})
	if err != nil {
		return nil, err
	}
	for i := range cards {
		cards[i].Body = cutText(cards[i].Body, listBodyLen)
	}
	_, res, err := fitting(len(cards), func(n int) any {
		if n == len(cards) {
			return map[string]any{"cards": cards[:n]}
		}
		return map[string]any{"cards": cards[:n], "truncated": true,
			"notice": "more cards than fit: narrow the list with column, assignee, blocked, labels, due dates, overdue or text"}
	})
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
	_, bodyCut := cut(t.Body, threadBodyLen)
	t.Body = cutText(t.Body, threadBodyLen)
	for i := range t.Comments {
		t.Comments[i].Body = cutText(t.Comments[i].Body, commentLen)
	}
	all := t.Comments
	reply := func(c boards.Card, m cutMarks, n int) any {
		shown := t
		shown.Card = c
		shown.Comments = all[len(all)-n:]
		return threadReply{Thread: shown, cutMarks: m, Omitted: len(all) - n}
	}
	// The card is cut first, to what fits with no comment at all...
	card, marks, err := fitCard(t.Card, func(c boards.Card, m cutMarks) any {
		m.BodyTruncated = m.BodyTruncated || bodyCut // the 16 KiB cut is in the reply too
		return reply(c, m, 0)
	})
	if err != nil {
		return nil, err
	}
	marks.BodyTruncated = marks.BodyTruncated || bodyCut
	// ...then the newest comments that fit.
	_, res, err := fitting(len(all), func(n int) any { return reply(card, marks, n) })
	return res, err
}

// cutMarks say what a reply left out to fit in a result.
type cutMarks struct {
	BodyTruncated bool `json:"body_truncated,omitempty"`
	DocsTruncated bool `json:"docs_truncated,omitempty"`
}

// threadReply is show_card's result: the card with its newest comments, and what was left out.
type threadReply struct {
	boards.Thread
	cutMarks
	Omitted int `json:"older_comments_omitted,omitempty"`
}

// cardReply is a card changed by a tool, its body and links cut if it would not fit in a result.
type cardReply struct {
	boards.Card
	cutMarks
}

// cardResult shapes the result of a tool that changed a card, after the change was made:
// a body of 32 KiB, or 50 doc paths, can be more than MaxResult once escaped as JSON, and
// the caller must still be told the card changed, so the body, then the last docs, are
// cut, with body_truncated and docs_truncated: true.
func cardResult(c boards.Card, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	c, marks, err := fitCard(c, func(c boards.Card, m cutMarks) any { return cardReply{c, m} })
	if err != nil {
		return nil, err
	}
	return cardReply{c, marks}, nil
}

// fitCard returns c with fewer docs, then a shorter body, until build(c, marks) marshals
// to at most MaxResult bytes, and what it cut (the docs that fit leave room for a body cut to its marker). build gets all the marks it may be given
// while measuring, so the size it fits includes them.
func fitCard(c boards.Card, build func(boards.Card, cutMarks) any) (boards.Card, cutMarks, error) {
	fits := func(c boards.Card, m cutMarks) (bool, error) {
		out, err := json.Marshal(build(c, m))
		return len(out) <= MaxResult, err
	}
	if ok, err := fits(c, cutMarks{}); ok || err != nil {
		return c, cutMarks{}, err
	}
	all := cutMarks{BodyTruncated: true, DocsTruncated: true}
	var marks cutMarks
	// The docs that fit with the least body...
	docs := c.Docs
	bare := c
	bare.Body = truncatedNotes // the least a cut body is
	if ok, err := fits(bare, all); err != nil {
		return c, cutMarks{}, err
	} else if !ok {
		lo, hi := 0, len(docs) // docs[:hi] doesn't fit; docs[:lo] fits, if anything does
		for lo < hi-1 {
			mid := (lo + hi) / 2
			bare.Docs = docs[:mid]
			ok, err := fits(bare, all)
			if err != nil {
				return c, cutMarks{}, err
			}
			if ok {
				lo = mid
			} else {
				hi = mid
			}
		}
		c.Docs = docs[:lo]
		marks.DocsTruncated = true
	}
	// ...then the start of the body that fits with them.
	body, bodyCut, err := fitBody(c.Body, func(b string) any {
		shown := c
		shown.Body = b
		return build(shown, all)
	})
	if err != nil {
		return c, cutMarks{}, err
	}
	c.Body = body
	marks.BodyTruncated = bodyCut
	return c, marks, nil
}

// commentReply is a comment added by comment_card, its body cut if it would not fit.
type commentReply struct {
	boards.Comment
	BodyTruncated bool `json:"body_truncated,omitempty"`
}

// fitBody returns body, or the longest start of it, marked as cut, for which build(body)
// marshals to at most MaxResult bytes, and whether it cut.
func fitBody(body string, build func(body string) any) (string, bool, error) {
	fits := func(b string) (bool, error) {
		out, err := json.Marshal(build(b))
		return len(out) <= MaxResult, err
	}
	if ok, err := fits(body); ok || err != nil {
		return body, false, err
	}
	cutTo := func(n int) string {
		short, _ := cut(body, n)
		return short + truncatedNotes
	}
	lo, hi := 0, len(body) // cutTo(hi) is too long; cutTo(lo) fits if anything does
	for lo < hi-1 {
		mid := (lo + hi) / 2
		ok, err := fits(cutTo(mid))
		if err != nil {
			return "", false, err
		}
		if ok {
			lo = mid
		} else {
			hi = mid
		}
	}
	return cutTo(lo), true, nil
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
	return cardResult(s.o.Boards.CreateCard(ctx, actor(c), a.SpaceID, a.BoardID, a.NewCard))
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
	return cardResult(s.o.Boards.UpdateCard(ctx, actor(c), a.SpaceID, a.BoardID, a.CardID, a.CardChanges))
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
	return cardResult(s.o.Boards.MoveCard(ctx, actor(c), a.SpaceID, a.BoardID, a.CardID, a.Move))
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
	cm, err := s.o.Boards.Comment(ctx, actor(c), a.SpaceID, a.BoardID, a.CardID, a.Body)
	if err != nil {
		return nil, err
	}
	body, wasCut, err := fitBody(cm.Body, func(b string) any {
		shown := cm
		shown.Body = b
		return commentReply{shown, true}
	})
	if err != nil {
		return nil, err
	}
	cm.Body = body
	return commentReply{cm, wasCut}, nil
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
	return cardResult(s.o.Boards.SetBlocked(ctx, actor(c), a.SpaceID, a.BoardID, a.CardID, a.Version, a.Reason, a.BlockedBy))
}

func (s *Service) createBoard(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID int64  `json:"space_id"`
		Name    string `json:"name"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.boardSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	return s.o.Boards.CreateBoard(ctx, actor(c), a.SpaceID, a.Name)
}

func (s *Service) updateBoard(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		ref
		boards.BoardChanges
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.boardSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	return s.o.Boards.UpdateBoard(ctx, actor(c), a.SpaceID, a.BoardID, a.BoardChanges)
}

func (s *Service) archiveCard(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		cardRef
		Version int64 `json:"version"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.boardSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	return cardResult(s.o.Boards.ArchiveCard(ctx, actor(c), a.SpaceID, a.BoardID, a.CardID, a.Version))
}

func (s *Service) restoreCard(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		cardRef
		Version int64         `json:"version"`
		Column  boards.Column `json:"column"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := s.boardSpace(ctx, c, a.SpaceID); err != nil {
		return nil, err
	}
	return cardResult(s.o.Boards.RestoreCard(ctx, actor(c), a.SpaceID, a.BoardID, a.CardID, a.Version, a.Column))
}

// cutText shortens s to n bytes, marking the cut.
func cutText(s string, n int) string {
	if short, cut := cut(s, n); cut {
		return short + truncatedNotes
	}
	return s
}
