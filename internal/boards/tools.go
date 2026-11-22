package boards

import (
	"context"
	"errors"

	"taskto.ai/taskto/internal/access"
)

// The agent's board tools (docs/boards-agent-tools.md) call the methods below. The agent
// works for one user at a time, in that user's conversation, and has exactly that user's
// access: each method checks a.UserID's access to the space through package access, as
// RequireSpace does for a request. A space, board or card the user can't read is
// ErrNotFound; a change they can't make is ErrReadOnly or ErrBoardArchived. Changes are
// recorded as a.UserID's, marked as the agent's when a.Agent is set.

// space returns spaceID as a's user sees it, or ErrNotFound.
func (s *Service) space(ctx context.Context, a Actor, spaceID int64) (access.Space, error) {
	sp, err := s.access.Check(ctx, a.UserID, spaceID)
	if errors.Is(err, access.ErrNotFound) {
		return access.Space{}, ErrNotFound
	}
	return sp, err
}

// ListBoards lists a space's boards that aren't archived: its personal board, or its team
// boards, by name.
func (s *Service) ListBoards(ctx context.Context, a Actor, spaceID int64) ([]Board, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return nil, err
	}
	return s.listBoards(ctx, sp, false)
}

// ListCards lists the cards of a board that f selects, in board order.
func (s *Service) ListCards(ctx context.Context, a Actor, spaceID, boardID int64, f Filter) ([]Card, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return nil, err
	}
	return s.listCards(ctx, sp, boardID, f)
}

// ShowCard returns a card with its thread.
func (s *Service) ShowCard(ctx context.Context, a Actor, spaceID, boardID, cardID int64) (Thread, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return Thread{}, err
	}
	return s.getThread(ctx, sp, boardID, cardID)
}

// CreateCard adds a card to a board.
func (s *Service) CreateCard(ctx context.Context, a Actor, spaceID, boardID int64, nc NewCard) (Card, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return Card{}, err
	}
	return s.createCard(ctx, sp, a, boardID, nc)
}

// UpdateCard edits a card: ch.Version must be its current version, or ErrStale.
func (s *Service) UpdateCard(ctx context.Context, a Actor, spaceID, boardID, cardID int64, ch CardChanges) (Card, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return Card{}, err
	}
	return s.updateCard(ctx, sp, a, boardID, cardID, ch)
}

// MoveCard moves a card to a column and position: m.Version must be its current
// version, or ErrStale.
func (s *Service) MoveCard(ctx context.Context, a Actor, spaceID, boardID, cardID int64, m Move) (Card, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return Card{}, err
	}
	return s.moveCard(ctx, sp, a, boardID, cardID, m)
}

// Comment adds a comment to a card's thread. It doesn't change the card's version.
func (s *Service) Comment(ctx context.Context, a Actor, spaceID, boardID, cardID int64, body string) (Comment, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return Comment{}, err
	}
	return s.comment(ctx, sp, a, boardID, cardID, body)
}

// SetBlocked sets why a card is blocked and the cards it waits for (of the same board):
// a nil reason and no cards unblock it. version must be the card's current version, or
// ErrStale.
func (s *Service) SetBlocked(ctx context.Context, a Actor, spaceID, boardID, cardID, version int64, reason *string, by []int64) (Card, error) {
	if by == nil {
		by = []int64{}
	}
	return s.UpdateCard(ctx, a, spaceID, boardID, cardID, CardChanges{
		Version:       version,
		BlockedReason: Optional[string]{Set: true, Value: reason},
		BlockedBy:     &by,
	})
}
