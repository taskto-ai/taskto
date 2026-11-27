package conversations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"taskto.ai/taskto/internal/agentrt"
)

type Sidecar interface {
	Conversations(ctx context.Context) ([]agentrt.ConversationInfo, error)
	RetireConversation(ctx context.Context, conversation int64) error
}

const OrphanGrace = 10 * time.Minute

type Lost struct {
	ID          int64
	WorkspaceID int64
	OwnerID     int64
	AgentID     int64
	CreatedAt   string
}

type Orphan struct {
	AgentID   int64
	CreatedAt time.Time
}

type Drift struct {
	Lost     []Lost
	Orphans  []Orphan
	Recorded int
}

func (d Drift) AllLost() bool { return d.Recorded > 0 && len(d.Lost) == d.Recorded }

func CheckDrift(ctx context.Context, sqlDB *sql.DB, rt Sidecar, now time.Time) (Drift, error) {
	recorded := map[int64]Lost{}
	rows, err := sqlDB.QueryContext(ctx, `SELECT id, workspace_id, owner_id, agent_id, created_at FROM conversations ORDER BY id`)
	if err != nil {
		return Drift{}, err
	}
	var order []int64
	for rows.Next() {
		var l Lost
		if err := rows.Scan(&l.ID, &l.WorkspaceID, &l.OwnerID, &l.AgentID, &l.CreatedAt); err != nil {
			rows.Close()
			return Drift{}, err
		}
		recorded[l.AgentID] = l
		order = append(order, l.AgentID)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return Drift{}, err
	}

	have, err := rt.Conversations(ctx)
	if err != nil {
		return Drift{}, fmt.Errorf("listing the agent runtime's conversations: %w", err)
	}
	live := map[int64]bool{}
	d := Drift{Recorded: len(order)}
	for _, c := range have {
		if c.Retired {
			continue
		}
		live[c.ID] = true
		if _, ok := recorded[c.ID]; ok {
			continue
		}
		var created time.Time
		if c.CreatedAt > 0 {
			created = time.UnixMilli(c.CreatedAt)
		}
		if now.Sub(created) >= OrphanGrace {
			d.Orphans = append(d.Orphans, Orphan{AgentID: c.ID, CreatedAt: created})
		}
	}
	for _, agentID := range order {
		if !live[agentID] {
			d.Lost = append(d.Lost, recorded[agentID])
		}
	}
	return d, nil
}

func RemoveLost(ctx context.Context, sqlDB *sql.DB, lost []Lost) (n int, err error) {
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	for _, l := range lost {
		res, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE id = ? AND agent_id = ?`, l.ID, l.AgentID)
		if err != nil {
			return 0, err
		}
		k, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		n += int(k)
	}
	return n, tx.Commit()
}

func RetireOrphans(ctx context.Context, sqlDB *sql.DB, rt Sidecar, orphans []Orphan) (int, error) {
	n := 0
	for _, o := range orphans {
		var recorded bool
		err := sqlDB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM conversations WHERE agent_id = ?)`, o.AgentID).Scan(&recorded)
		if err != nil {
			return n, err
		}
		if recorded {
			continue
		}
		var se *agentrt.StatusError
		switch err := rt.RetireConversation(ctx, o.AgentID); {
		case errors.As(err, &se) && se.Code == http.StatusNotFound:
		case err != nil:
			return n, fmt.Errorf("retiring the agent runtime's conversation %d: %w", o.AgentID, err)
		default:
			n++
		}
	}
	return n, nil
}
