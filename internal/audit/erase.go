package audit

import (
	"context"
	"database/sql"
)

// Erase is what deleting an account does to the log (docs/audit.md#deleted-accounts), run in the
// transaction that deletes the user, before it does.
//
// The events the user did lose their actor with the account (actor_id is ON DELETE SET NULL),
// which would read like an anonymous attempt, so they are marked as a deleted account's. The events
// about the account (target_type "user", with the account's creation time, since an id is given
// again) are marked too. The invitations to the account's address, which carry the address in their
// details (an invitation is about a person who had no account yet), lose it: an address is the one
// thing about a person that a detail may hold. It returns how many events it changed.
//
// Nothing else in an event is about a person: details are typed (schema.go), and name no one but by
// the id of an account or a space.
func Erase(ctx context.Context, tx *sql.Tx, userID int64, createdAt, email string) (int64, error) {
	var changed int64
	for _, q := range []struct {
		stmt string
		args []any
	}{
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
