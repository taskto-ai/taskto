package access

import (
	"context"
	"database/sql"
	"errors"
)

func SpaceRowCheck(db *sql.DB) func(s Space) (bool, error) {
	return func(s Space) (bool, error) {
		var one int
		err := db.QueryRowContext(context.Background(),
			`SELECT 1 FROM spaces WHERE id = ? AND workspace_id = ? AND (? = '' OR created_at = ?)`,
			s.ID, s.WorkspaceID, s.CreatedAt, s.CreatedAt).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
}
