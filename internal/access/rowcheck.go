package access

import (
	"context"
	"database/sql"
	"errors"
)

// SpaceRowCheck returns a check of whether a space's row is still there and is the space s was
// read from: the same ID and workspace and, when s has it, the same creation time. A space made
// later with a purged space's ID (a row's ID is the table's largest plus one, so it can be reused)
// has another creation time, so a request that was authorized for the old one is refused. It is
// for code that is about to create files in a space's directory, which must not make that
// directory for a space that is gone (knowledge.Store.SetSpaceCheck).
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
