package audit

import (
	"context"
	"database/sql"
)

func Erase(ctx context.Context, tx *sql.Tx, userID int64, createdAt, email string) (int64, error) {
	var changed int64
	for _, q := range []struct {
		stmt string
		args []any
	}{
		{`UPDATE audit_events SET client_addr = NULL WHERE actor_id = ? AND client_addr IS NOT NULL`, []any{userID}},
		{`UPDATE audit_events SET actor_deleted = 1 WHERE actor_id = ?`, []any{userID}},
		{`UPDATE audit_events SET target_deleted = 1
			WHERE target_type = ? AND target_id = ? AND target_created_at IS ?`, []any{TargetUser, userID, createdAt}},
		{`UPDATE audit_events SET details = json_remove(details, '$.email')
			WHERE action IN (?, ?) AND lower(json_extract(details, '$.email')) = lower(?)`, []any{InviteCreated, InviteRevoked, email}},
	} {
		res, err := tx.ExecContext(ctx, q.stmt, q.args...)
		if err != nil {
			return changed, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return changed, err
		}
		changed += n
	}
	return changed, nil
}
