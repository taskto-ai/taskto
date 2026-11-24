package boards

import (
	"context"
	"errors"
)

// exportPage is how many cards the export reads the IDs of at a time.
var exportPage = 100

// ExportBoards returns the boards of spaceID, archived ones too, for the data export
// (docs/api.md#export-my-data). It reads as userID: a space they can't read is ErrNotFound, as for
// the agent's tools.
func (s *Service) ExportBoards(ctx context.Context, userID, spaceID int64) ([]Board, error) {
	sp, err := s.space(ctx, Actor{UserID: userID}, spaceID)
	if err != nil {
		return nil, err
	}
	return s.listBoards(ctx, sp, true)
}

// ExportCards calls visit with each card of board boardID in spaceID with its thread, archived
// ones too, in the order of their IDs. The cards are found a page of IDs at a time, each page one
// statement over every column (so a card that moves to another column, or is archived, meanwhile
// is still read once), and each is read whole as it is called for: what is held at a time is a
// page of IDs and one card's thread, however many cards the board has. A card that was deleted
// since its ID was read is left out. It reads as userID, as ExportBoards does.
func (s *Service) ExportCards(ctx context.Context, userID, spaceID, boardID int64, visit func(Thread) error) error {
	sp, err := s.space(ctx, Actor{UserID: userID}, spaceID)
	if err != nil {
		return err
	}
	var after int64
	for {
		ids, err := s.cardIDs(ctx, spaceID, boardID, after, exportPage)
		if err != nil {
			return err
		}
		for _, id := range ids {
			t, err := s.getThread(ctx, sp, boardID, id)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if err := visit(t); err != nil {
				return err
			}
		}
		if len(ids) < exportPage {
			return nil
		}
		after = ids[len(ids)-1]
	}
}

// cardIDs returns up to limit IDs of the cards of the board in the space, after the ID after, in
// order: one statement over all of the board's columns.
func (s *Service) cardIDs(ctx context.Context, spaceID, boardID, after int64, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id FROM cards c JOIN boards b ON b.id = c.board_id
		WHERE c.board_id = ? AND b.space_id = ? AND c.id > ? ORDER BY c.id LIMIT ?`, boardID, spaceID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
