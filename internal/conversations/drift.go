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

// The server and the agent runtime can drift apart in two ways, which CheckDrift finds:
//
//   - A lost conversation is one the server recorded but the agent runtime doesn't have:
//     its data was wiped, or restored apart from the server's. Its routes answer 410
//     (conversationError). Its content is gone, so cleaning up deletes the row.
//   - An orphan is a conversation the agent runtime created but the server never recorded,
//     because recording it failed (handleCreate logs it). Nobody can reach it. Pi Durable
//     never deletes a conversation, so cleaning up retires it: its work stops and the agent
//     runtime refuses it from then on.
//
// `taskto conversations` (cmd/taskto) runs these for the operator against the running
// server's agent runtime.

// Sidecar is what the drift check needs of the agent runtime: an *agentrt.Client, or a fake
// in tests.
type Sidecar interface {
	Conversations(ctx context.Context) ([]agentrt.ConversationInfo, error)
	RetireConversation(ctx context.Context, conversation int64) error
}

// OrphanGrace is how old an unrecorded conversation of the agent runtime must be to count as
// an orphan: a younger one may be one the server is recording right now.
const OrphanGrace = 10 * time.Minute

// Lost is a recorded conversation the agent runtime doesn't have. Its title isn't shown:
// conversations are private to their owner.
type Lost struct {
	ID          int64
	WorkspaceID int64
	OwnerID     int64
	AgentID     int64
	CreatedAt   string
}

// Orphan is a conversation of the agent runtime that no recorded conversation refers to.
type Orphan struct {
	AgentID int64
	// CreatedAt is zero for one made before the agent runtime recorded the time.
	CreatedAt time.Time
}

// Drift is what CheckDrift found.
type Drift struct {
	Lost    []Lost
	Orphans []Orphan
	// Recorded is how many conversations the server had recorded.
	Recorded int
}

// AllLost reports whether every recorded conversation is lost: what a wiped agent runtime
// looks like, but also a listing that came back empty by mistake. Deleting them all takes
// the operator's explicit say.
func (d Drift) AllLost() bool { return d.Recorded > 0 && len(d.Lost) == d.Recorded }

// CheckDrift compares the conversations recorded in sqlDB with the agent runtime's, as of
// now. The rows are read before the agent runtime is asked, and a row is only ever written
// for a conversation the agent runtime has already created, so a conversation being created
// meanwhile is never taken for lost; OrphanGrace keeps it from being taken for an orphan.
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
			continue // the agent runtime refuses it: lost, if a row still refers to it
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

// RemoveLost deletes the rows of lost conversations, all or none, and returns how many it
// deleted. A row is only deleted if it still refers to the same agent runtime conversation.
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

// RetireOrphans retires the agent runtime's orphaned conversations, and returns how many it
// retired. It checks again that no row refers to each one right before retiring it, and
// skips one the agent runtime no longer has.
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
