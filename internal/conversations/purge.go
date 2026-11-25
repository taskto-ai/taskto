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
// owned. The agent runtime never deletes a conversation (Pi Durable keeps its records), so the
// server retires each one first: its work stops and the runtime refuses it from then on, as for one
// that never existed, and then deletes its own row, which is the only thing that says whose it was.
// The runtime's records of what was said stay in its own database, unreachable and tied to no
// one (docs/install.md#privacy-and-account-deletion).

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

// RetireOwned retires the agent runtime's conversation of each, and returns how many it retired:
// one the runtime no longer has (404) counts as done, and isn't counted. Retiring is idempotent,
// so a purge that stops halfway does it again.
func RetireOwned(ctx context.Context, rt Sidecar, owned []Owned) (int, error) {
	n := 0
	for _, o := range owned {
		var se *agentrt.StatusError
		switch err := rt.RetireConversation(ctx, o.AgentID); {
		case err == nil:
			n++
		case errors.As(err, &se) && se.Code == http.StatusNotFound:
		default:
			return n, fmt.Errorf("retiring the agent runtime's conversation %d: %w", o.AgentID, err)
		}
	}
	return n, nil
}

// DeleteOwned deletes owner's conversations' rows (and, with them, the CLI sessions recorded for
// each), and returns how many. Call it in the transaction that deletes the account, after
// RetireOwned.
func DeleteOwned(ctx context.Context, tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, owner int64) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE owner_id = ?`, owner)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
