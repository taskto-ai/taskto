package boards

import (
	"context"
	"database/sql"
	"errors"

	"taskto.ai/taskto/internal/access"
)

func (s *Service) space(ctx context.Context, a Actor, spaceID int64) (access.Space, error) {
	sp, err := s.access.Check(ctx, a.UserID, spaceID)
	if errors.Is(err, access.ErrNotFound) {
		return access.Space{}, ErrNotFound
	}
	return sp, err
}

func (s *Service) ListBoards(ctx context.Context, a Actor, spaceID int64) ([]Board, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return nil, err
	}
	return s.listBoards(ctx, sp, false)
}

func (s *Service) ListAllBoards(ctx context.Context, a Actor, spaceID int64) ([]Board, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return nil, err
	}
	return s.listBoards(ctx, sp, true)
}

func (s *Service) CreateBoard(ctx context.Context, a Actor, spaceID int64, name string) (Board, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return Board{}, err
	}
	return s.createBoard(ctx, sp, a, name)
}

func (s *Service) UpdateBoard(ctx context.Context, a Actor, spaceID, boardID int64, ch BoardChanges) (Board, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return Board{}, err
	}
	return s.updateBoard(ctx, sp, a, boardID, ch)
}

func (s *Service) ArchiveCard(ctx context.Context, a Actor, spaceID, boardID, cardID, version int64) (Card, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return Card{}, err
	}
	return s.archiveCard(ctx, sp, a, boardID, cardID, version)
}

func (s *Service) RestoreCard(ctx context.Context, a Actor, spaceID, boardID, cardID, version int64, column Column) (Card, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return Card{}, err
	}
	return s.restoreCard(ctx, sp, a, boardID, cardID, version, column)
}

func (s *Service) ListCards(ctx context.Context, a Actor, spaceID, boardID int64, f Filter) ([]Card, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return nil, err
	}
	return s.listCards(ctx, sp, boardID, f)
}

func (s *Service) ShowCard(ctx context.Context, a Actor, spaceID, boardID, cardID int64) (Thread, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return Thread{}, err
	}
	return s.getThread(ctx, sp, a, boardID, cardID)
}

func (s *Service) CreateCard(ctx context.Context, a Actor, spaceID, boardID int64, nc NewCard) (Card, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return Card{}, err
	}
	return s.createCard(ctx, sp, a, boardID, nc)
}

func (s *Service) UpdateCard(ctx context.Context, a Actor, spaceID, boardID, cardID int64, ch CardChanges) (Card, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return Card{}, err
	}
	return s.updateCard(ctx, sp, a, boardID, cardID, ch)
}

func (s *Service) MoveCard(ctx context.Context, a Actor, spaceID, boardID, cardID int64, m Move) (Card, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return Card{}, err
	}
	return s.moveCard(ctx, sp, a, boardID, cardID, m)
}

func (s *Service) MoveCardIf(ctx context.Context, a Actor, spaceID, boardID, cardID int64, column Column,
	guard func(tx *sql.Tx, c Card) (bool, error),
) (Card, bool, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return Card{}, false, err
	}
	return s.moveCardIf(ctx, sp, a, boardID, cardID, column, guard)
}

func (s *Service) Comment(ctx context.Context, a Actor, spaceID, boardID, cardID int64, body string) (Comment, error) {
	sp, err := s.space(ctx, a, spaceID)
	if err != nil {
		return Comment{}, err
	}
	return s.comment(ctx, sp, a, boardID, cardID, body)
}

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
