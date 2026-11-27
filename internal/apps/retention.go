package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

const (
	DefaultDeletedRetention = 90 * 24 * time.Hour

	MaxRevisions = 500

	retentionBatch  = 200
	retentionBudget = 5 * time.Minute
)

func (s *Service) WithDeletedRetention(d time.Duration) *Service {
	s.deletedRetention = d
	return s
}

func (s *Service) DeletedRetention() time.Duration { return s.deletedRetention }

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
		if _, err := tx.ExecContext(ctx, `UPDATE app_row_ids SET high = max(high, coalesce((SELECT max(id) FROM app_rows), 0))`); err != nil {
			return err
		}
		list, err := json.Marshal(ids)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE app_operations SET record_id = NULL WHERE record_id IN (SELECT value FROM json_each(?))`, string(list)); err != nil {
			return err
		}
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
