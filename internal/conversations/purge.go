package conversations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"taskto.ai/taskto/internal/agentrt"
)

type Owned struct {
	ID      int64
	AgentID int64
}

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

type Purger interface {
	Sidecar
	PurgeConversation(ctx context.Context, conversation int64) error
}

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

func DeleteOwned(ctx context.Context, tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, owner int64) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE owner_id = ?`, owner)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
