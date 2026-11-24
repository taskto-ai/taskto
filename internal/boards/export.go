package boards

import "context"

// Export calls visit with each board of spaceID, archived ones too, and then with each of its
// cards, archived ones too, with their threads, in board order. It is for the data export
// (docs/api.md#export-my-data) and reads as userID: a space they can't read is ErrNotFound,
// as for the agent's tools. visitCard is called after visitBoard of its board.
func (s *Service) Export(ctx context.Context, userID, spaceID int64, visitBoard func(Board) error, visitCard func(Board, Thread) error) error {
	sp, err := s.space(ctx, Actor{UserID: userID}, spaceID)
	if err != nil {
		return err
	}
	boards, err := s.listBoards(ctx, sp, true)
	if err != nil {
		return err
	}
	for _, b := range boards {
		if err := visitBoard(b); err != nil {
			return err
		}
		live, err := s.listCards(ctx, sp, b.ID, Filter{})
		if err != nil {
			return err
		}
		archived, err := s.listCards(ctx, sp, b.ID, Filter{Column: Archived})
		if err != nil {
			return err
		}
		for _, c := range append(live, archived...) {
			t, err := s.getThread(ctx, sp, b.ID, c.ID)
			if err != nil {
				return err
			}
			if err := visitCard(b, t); err != nil {
				return err
			}
		}
	}
	return nil
}
