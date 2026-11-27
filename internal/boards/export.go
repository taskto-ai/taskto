package boards

import (
	"context"
	"database/sql"
	"errors"

	"taskto.ai/taskto/internal/access"
)

var exportPage = 100

var errCardGone = errors.New("the card is gone")

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
