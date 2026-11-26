package conversations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"taskto.ai/taskto/internal/agentrt"
)

// The purge of a deleted account (internal/accountdeletion) deletes the conversations the person
// owned. The agent runtime has no way to delete a conversation (Pi Durable keeps its records), so
// the server retires each one first: its work stops and the runtime refuses it from then on, as
// for one that never existed. The runtime then purges what it kept of it in its own database (what
// was said, its tasks, its documents), and the server deletes its own row, which is the only thing
// that says whose it was. Both steps are the runtime's to do, and are repeated until they are done
// (the purge keeps a record of each until then, docs/install.md#privacy-and-account-deletion).

// Owned is a conversation a user owns, as the purge needs it.
type Owned struct {
	ID      int64
	AgentID int64
}

// OwnedBy lists the conversations owner owns, in every workspace.
func OwnedBy(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, owner int64) ([]Owned, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, agent_id FROM conversations WHERE owner_id = ? ORDER BY id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Owned
	for rows.Next() {
		var o Owned
		if err := rows.Scan(&o.ID, &o.AgentID); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Purger is the agent runtime as the purge of an account needs it: a Sidecar that can also purge
// a retired conversation's records (an *agentrt.Client).
type Purger interface {
	Sidecar
	PurgeConversation(ctx context.Context, conversation int64) error
}

// PurgeConversation retires the runtime's conversation agentID, if it isn't already, and purges its
// records from the runtime's database. A conversation the runtime no longer has is done. It is
// idempotent, so a step that stopped halfway is done again.
func PurgeConversation(ctx context.Context, rt Purger, agentID int64) error {
	var se *agentrt.StatusError
	if err := rt.RetireConversation(ctx, agentID); err != nil {
		if !errors.As(err, &se) || se.Code != http.StatusNotFound {
			return fmt.Errorf("retiring the agent runtime's conversation %d: %w", agentID, err)
		}
	}
	if err := rt.PurgeConversation(ctx, agentID); err != nil {
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("purging the agent runtime's conversation %d: %w", agentID, err)
	}
	return nil
}

// DeleteOwned deletes owner's conversations' rows (and, with them, the CLI sessions recorded for
// each), and returns how many. Call it in the transaction that deletes the account, after
// PurgeConversation.
func DeleteOwned(ctx context.Context, tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, owner int64) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE owner_id = ?`, owner)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
