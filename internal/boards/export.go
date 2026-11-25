package boards

import (
	"context"
	"database/sql"
	"errors"

	"taskto.ai/taskto/internal/access"
)

// exportPage is how many boards or cards the export reads the IDs of at a time.
var exportPage = 100

// errCardGone is a card deleted since its ID was read, which the export leaves out; it is never
// returned, so a space that can't be read any more (ErrNotFound) is told apart from it.
var errCardGone = errors.New("the card is gone")

// exportRead runs fn in one snapshot (access.Snapshot) that begins with the check that userID may
// read spaceID: what fn reads is of the same state that said they may, and a person who lost the
// space gets ErrNotFound and nothing of it. The export asks it again for every page and every
// card, so what it holds of a space is never read after the person lost it.
func (s *Service) exportRead(ctx context.Context, userID, spaceID int64, fn func(tx *sql.Tx, sp access.Space) error) error {
	return s.access.Snapshot(ctx, func(snap *access.Checker, tx *sql.Tx) error {
		sp, err := snap.Check(ctx, userID, spaceID)
		if errors.Is(err, access.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return fn(tx, sp)
	})
}

// ExportBoards calls visit with each board of spaceID, archived ones too, in the order of their
// IDs, for the data exports (docs/api.md#export-my-data). The boards are read a page at a time,
// each page in a snapshot that begins with the check of userID's access, and visit is called
// between snapshots: what is held is a page of boards, however many the space has. It reads as
// userID: a space they can't read, or that they lose part-way through, is ErrNotFound, as for the
// agent's tools.
func (s *Service) ExportBoards(ctx context.Context, userID, spaceID int64, visit func(Board) error) error {
	var after int64
	for {
		var page []Board
		err := s.exportRead(ctx, userID, spaceID, func(tx *sql.Tx, sp access.Space) error {
			if err := may(sp, access.Read); err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, `SELECT `+boardColumns+` WHERE b.space_id = ? AND b.id > ? ORDER BY b.id LIMIT ?`,
				sp.ID, after, exportPage)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				b, err := scanBoard(rows)
				if err != nil {
					return err
				}
				page = append(page, b)
			}
			return rows.Err()
		})
		if err != nil {
			return err
		}
		for _, b := range page {
			if err := visit(b); err != nil {
				return err
			}
		}
		if len(page) < exportPage {
			return nil
		}
		after = page[len(page)-1].ID
	}
}

// ExportCards calls visit with each card of board boardID in spaceID with its thread, archived
// ones too, in the order of their IDs. The cards are found a page of IDs at a time, each page one
// statement over every column (so a card that moves to another column, or is archived, meanwhile
// is still read once), and each is read whole as it is called for: what is held at a time is a
// page of IDs and one card's thread, however many cards the board has. A card that was deleted
// since its ID was read is left out. It reads as userID, as ExportBoards does: every page of IDs
// and every card is read in a snapshot that begins with the check of their access, so a card is
// never read after they lost the space.
func (s *Service) ExportCards(ctx context.Context, userID, spaceID, boardID int64, visit func(Thread) error) error {
	var after int64
	for {
		var ids []int64
		err := s.exportRead(ctx, userID, spaceID, func(tx *sql.Tx, sp access.Space) (err error) {
			if err := may(sp, access.Read); err != nil {
				return err
			}
			ids, err = cardIDs(ctx, tx, spaceID, boardID, after, exportPage)
			return err
		})
		if err != nil {
			return err
		}
		for _, id := range ids {
			var t Thread
			var sp access.Space
			err := s.exportRead(ctx, userID, spaceID, func(tx *sql.Tx, space access.Space) (err error) {
				if err := may(space, access.Read); err != nil {
					return err
				}
				sp = space
				t, err = threadIn(ctx, tx, spaceID, boardID, id)
				if errors.Is(err, ErrNotFound) {
					return errCardGone
				}
				return err
			})
			if errors.Is(err, errCardGone) {
				continue
			}
			if err != nil {
				return err
			}
			if err := s.withMentions(ctx, sp, userID, &t); err != nil {
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
func cardIDs(ctx context.Context, q queryer, spaceID, boardID, after int64, limit int) ([]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT c.id FROM cards c JOIN boards b ON b.id = c.board_id
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
