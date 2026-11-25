package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Retention of deleted records and their history.
//
// Deleting a record is soft: the row stays, with every revision of its history, so that it can be
// restored (records.go). Left alone, a loop of inserts and deletes, which a session, an access
// token or the agent can run at the speed of the API, would grow the database without bound
// while the app's limits, which count live rows, never notice. So a deleted record is kept for
// a window (DefaultDeletedRetention, or the operator's -app-deleted-retention) and then removed
// by PurgeDeleted, with its history, which goes with it.
//
// What the sweep keeps and what it doesn't:
//
//   - A record is removed only for being deleted: a live record, and its history, are never
//     touched by it. A restore inside the window is the same transaction as before, and one
//     after it finds no record (404), as for a record that never was.
//   - The receipts of operations (app_operations, the idempotency keys of offline replays) outlive
//     the record they made: the sweep sets the record aside (record_id becomes NULL), which the
//     outbox's own purge ages out by request (keepDone), so a device that replays an old request
//     is answered with its receipt as ever. Only the link of a download's records to their
//     operations is lost, and a purged record isn't in a download.
//   - A record with effects that wait or run is kept, so no effect is lost with its record. Its
//     done entries, and the link to the card made for it, go with it.
//   - Attachments are rows that refer to the record, so they go with it, and their files, which
//     no row knows then, are removed by Sweep (the stray-file part of the attachment sweep).
//   - Nothing is removed in one go: each pass takes at most retentionBatch records in a
//     transaction of its own, so other writers aren't held, and PurgeDeleted stops at its time
//     budget, resuming at the next call.
//
// The history of a live record is capped too: remember keeps its newest MaxRevisions versions,
// so a loop of edits of one record is bounded as well. The oldest versions, the first insert
// among them, are dropped as new ones are added.

const (
	// DefaultDeletedRetention is how long a deleted record stays restorable when the operator
	// sets nothing.
	DefaultDeletedRetention = 90 * 24 * time.Hour

	// MaxRevisions is how many versions of a record its history keeps.
	MaxRevisions = 500

	// retentionBatch is the most records one transaction of the sweep removes.
	retentionBatch = 200
	// retentionBudget is how long one PurgeDeleted works before it leaves the rest to the next
	// call.
	retentionBudget = 5 * time.Minute
)

// WithDeletedRetention sets how long deleted records are kept before PurgeDeleted removes them,
// and returns the service. Zero keeps them for ever. Without it, DefaultDeletedRetention.
func (s *Service) WithDeletedRetention(d time.Duration) *Service {
	s.deletedRetention = d
	return s
}

// DeletedRetention is how long a deleted record is kept: zero for ever.
func (s *Service) DeletedRetention() time.Duration { return s.deletedRetention }

// PurgeDeleted removes the records deleted longer ago than the retention window, with their
// history, and returns how many it removed. It does nothing if the window is off.
func (s *Service) PurgeDeleted(ctx context.Context) (int, error) {
	if s.deletedRetention <= 0 {
		return 0, nil
	}
	cutoff := s.stamp(s.clock().Add(-s.deletedRetention))
	ctx, cancel := context.WithTimeout(ctx, retentionBudget)
	defer cancel()
	total := 0
	for ctx.Err() == nil {
		n, err := s.purgeBatch(ctx, cutoff)
		total += n
		if err != nil {
			return total, err
		}
		if n < retentionBatch {
			break
		}
	}
	return total, nil
}

// purgeBatch removes up to retentionBatch of the records deleted before cutoff.
func (s *Service) purgeBatch(ctx context.Context, cutoff string) (int, error) {
	removed := 0
	err := s.write(ctx, func(tx *sql.Tx) error {
		removed = 0
		rows, err := tx.QueryContext(ctx, `
			SELECT r.id FROM app_rows r
			WHERE r.deleted_at IS NOT NULL AND r.deleted_at < ?
			  AND NOT EXISTS (SELECT 1 FROM app_effects e WHERE e.record_id = r.id AND e.state IN ('pending', 'running'))
			LIMIT ?`, cutoff, retentionBatch)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		list, err := json.Marshal(ids)
		if err != nil {
			return err
		}
		// The receipts stay; only their link to the record goes.
		if _, err := tx.ExecContext(ctx, `UPDATE app_operations SET record_id = NULL WHERE record_id IN (SELECT value FROM json_each(?))`, string(list)); err != nil {
			return err
		}
		// History, attachments, effects and their cards go with the record (ON DELETE CASCADE).
		res, err := tx.ExecContext(ctx, `DELETE FROM app_rows WHERE deleted_at IS NOT NULL AND id IN (SELECT value FROM json_each(?))`, string(list))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		removed = int(n)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("purging deleted records: %w", err)
	}
	return removed, nil
}
