package boards

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/schedule"
)

const (
	KindDueReminder schedule.Kind = "card.due-reminder"
	KindOverdue     schedule.Kind = "card.overdue"
)

const reminderHour = 8

type reminderPayload struct {
	Board int64  `json:"board"`
	Card  int64  `json:"card"`
	Due   string `json:"due"`
}

func (s *Service) WithScheduler(sch *schedule.Service) *Service {
	s.sched = sch
	return s
}

func (s *Service) EnableReminders(sch *schedule.Service) error {
	if err := sch.Register(KindDueReminder, s.remind(notifications.KindCardDue)); err != nil {
		return err
	}
	if err := sch.Register(KindOverdue, s.remind(notifications.KindCardOverdue)); err != nil {
		return err
	}
	s.sched = sch
	return nil
}

func reminderKey(cardID int64) string { return fmt.Sprintf("card:%d", cardID) }

func reminderTimes(due string, loc *time.Location) (remind, overdue time.Time, err error) {
	day, err := time.Parse(dateLayout, due)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	y, m, d := day.Date()
	remind = schedule.LocalHour(y, m, d, reminderHour, loc).UTC()
	overdue = schedule.LocalHour(y, m, d+1, reminderHour, loc).UTC()
	return remind, overdue, nil
}

func (s *Service) syncReminders(ctx context.Context, tx *sql.Tx, cardID, previous int64) error {
	return s.syncCard(ctx, tx, cardID, previous, nil)
}

type reminderCap map[int64]map[schedule.Kind]bool

func (c reminderCap) mark(user int64, kind schedule.Kind) {
	if c[user] == nil {
		c[user] = map[schedule.Kind]bool{}
	}
	c[user][kind] = true
}

func (c reminderCap) has(user int64, kind schedule.Kind) bool { return c[user][kind] }

func (c reminderCap) both(user int64) bool {
	return c.has(user, KindDueReminder) && c.has(user, KindOverdue)
}

func (s *Service) syncCard(ctx context.Context, tx *sql.Tx, cardID, previous int64, capped reminderCap) error {
	if s.sched == nil {
		return nil
	}
	var (
		col                           string
		due                           sql.NullString
		assignee                      sql.NullInt64
		agent                         bool
		boardArchived, spaceArchd     sql.NullString
		spaceID, workspaceID, boardID int64
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT c.col, c.due, c.assignee_id, c.assignee_agent, b.archived_at, s.archived_at, s.id, s.workspace_id, b.id
		FROM cards c JOIN boards b ON b.id = c.board_id JOIN spaces s ON s.id = b.space_id
		WHERE c.id = ?`, cardID).
		Scan(&col, &due, &assignee, &agent, &boardArchived, &spaceArchd, &spaceID, &workspaceID, &boardID); err != nil {
		return err
	}
	var owner int64
	if assignee.Valid && !agent && due.Valid && col != string(Done) && col != string(Archived) &&
		!boardArchived.Valid && !spaceArchd.Valid {
		switch _, err := s.access.CheckTx(ctx, tx, assignee.Int64, spaceID); {
		case err == nil:
			owner = assignee.Int64
		case !errors.Is(err, access.ErrNotFound):
			return err
		}
	}
	for _, user := range []int64{previous, assignee.Int64} {
		if user != 0 && user != owner {
			if err := s.cancelReminders(ctx, tx, user, cardID); err != nil {
				return err
			}
		}
	}
	if owner == 0 {
		return nil
	}
	loc, err := auth.UserLocation(ctx, tx, owner)
	if err != nil {
		return err
	}
	remind, overdue, err := reminderTimes(due.String, loc)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(reminderPayload{Board: boardID, Card: cardID, Due: due.String})
	if err != nil {
		return err
	}
	now := s.sched.Now()
	for _, j := range []struct {
		kind schedule.Kind
		at   time.Time
	}{{KindDueReminder, remind}, {KindOverdue, overdue}} {
		job := schedule.Job{
			Kind: j.kind, UserID: owner, WorkspaceID: workspaceID, SpaceID: spaceID,
			DedupeKey: reminderKey(cardID), Payload: payload, RunAt: j.at,
		}
		switch existing, err := s.sched.Find(ctx, tx, owner, j.kind, job.DedupeKey); {
		case err == nil && existing.RunAt.Equal(j.at) && existing.SpaceID == spaceID && string(existing.Payload) == string(payload):
			continue
		case err != nil && !errors.Is(err, schedule.ErrNotFound):
			return err
		}
		if !j.at.After(now) || j.at.After(schedule.MaxRunAt) {
			if _, err := s.sched.Cancel(ctx, tx, owner, j.kind, job.DedupeKey); err != nil {
				return err
			}
			continue
		}
		if _, err := s.sched.Reschedule(ctx, tx, job); err != nil {
			if errors.Is(err, schedule.ErrTooMany) || errors.Is(err, schedule.ErrInvalid) {
				slog.Warn("boards: a reminder was not scheduled", "user", owner, "err", err)
				if errors.Is(err, schedule.ErrTooMany) && capped != nil {
					capped.mark(owner, j.kind)
				}
				continue
			}
			return err
		}
	}
	return nil
}

func (s *Service) cancelReminders(ctx context.Context, tx *sql.Tx, userID, cardID int64) error {
	for _, k := range []schedule.Kind{KindDueReminder, KindOverdue} {
		if _, err := s.sched.Cancel(ctx, tx, userID, k, reminderKey(cardID)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) syncCards(ctx context.Context, tx *sql.Tx, forUser int64, query string, args ...any) error {
	if s.sched == nil {
		return nil
	}
	rows, err := tx.QueryContext(ctx, query, args...)
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
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	capped := reminderCap{}
	for _, id := range ids {
		if err := s.syncCard(ctx, tx, id, 0, capped); err != nil {
			return err
		}
		if forUser != 0 && capped.both(forUser) {
			break
		}
	}
	return nil
}

const dated = `c.assignee_id IS NOT NULL AND NOT c.assignee_agent AND c.due IS NOT NULL`

const open = `c.col NOT IN ('done', 'archived')`

func (s *Service) syncBoard(ctx context.Context, tx *sql.Tx, boardID int64) error {
	return s.syncCards(ctx, tx, 0, `SELECT c.id FROM cards c WHERE c.board_id = ? AND `+dated+` ORDER BY c.id`, boardID)
}

func (s *Service) SyncSpace(ctx context.Context, tx *sql.Tx, spaceID int64) error {
	return s.syncCards(ctx, tx, 0, `
		SELECT c.id FROM cards c JOIN boards b ON b.id = c.board_id
		WHERE b.space_id = ? AND `+dated+` ORDER BY c.id`, spaceID)
}

func (s *Service) SyncSpaceMember(ctx context.Context, tx *sql.Tx, spaceID, userID int64) error {
	return s.syncCards(ctx, tx, userID, `
		SELECT c.id FROM cards c JOIN boards b ON b.id = c.board_id
		WHERE b.space_id = ? AND c.assignee_id = ? AND `+dated+` AND `+open+` ORDER BY c.id`, spaceID, userID)
}

func (s *Service) SyncWorkspaceMember(ctx context.Context, tx *sql.Tx, workspaceID, userID int64) error {
	return s.syncCards(ctx, tx, userID, `
		SELECT c.id FROM cards c JOIN boards b ON b.id = c.board_id JOIN spaces sp ON sp.id = b.space_id
		WHERE sp.workspace_id = ? AND c.assignee_id = ? AND `+dated+` AND `+open+` ORDER BY c.id`, workspaceID, userID)
}

func (s *Service) SyncTimeZone(ctx context.Context, tx *sql.Tx, userID int64) error {
	return s.syncCards(ctx, tx, 0, `
		SELECT c.id FROM cards c WHERE c.assignee_id = ? AND `+dated+` AND `+open+` ORDER BY c.id`, userID)
}

const reconcileBatch = 200

func (s *Service) ReconcileReminders(ctx context.Context) error {
	if s.sched == nil {
		return nil
	}
	from := s.sched.Now().UTC().AddDate(0, 0, -2).Format(dateLayout)
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id FROM cards c JOIN boards b ON b.id = c.board_id JOIN spaces s ON s.id = b.space_id
		WHERE `+dated+` AND c.due >= ? AND c.col NOT IN ('done', 'archived')
		  AND b.archived_at IS NULL AND s.archived_at IS NULL
		ORDER BY c.id`, from)
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
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for len(ids) > 0 {
		n := min(reconcileBatch, len(ids))
		batch := ids[:n]
		ids = ids[n:]
		if err := s.write(ctx, func(tx *sql.Tx) error {
			for _, id := range batch {
				if err := s.syncReminders(ctx, tx, id, 0); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) remind(notice string) schedule.Handler {
	return func(ctx context.Context, job schedule.Job) error {
		var p reminderPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return schedule.Permanent(fmt.Errorf("the payload is malformed: %w", err))
		}
		return s.write(ctx, func(tx *sql.Tx) error {
			if holds, err := s.sched.Holds(ctx, tx, job); err != nil || !holds {
				return err
			}
			sp, err := s.access.CheckTx(ctx, tx, job.UserID, job.SpaceID)
			switch {
			case errors.Is(err, access.ErrNotFound):
				return schedule.ErrAccessLost
			case err != nil:
				return err
			case sp.WorkspaceID != job.WorkspaceID:
				return schedule.ErrAccessLost
			case sp.ArchivedAt != nil:
				return nil
			}
			var (
				col      string
				due      sql.NullString
				assignee sql.NullInt64
				agent    bool
				archived sql.NullString
			)
			err = tx.QueryRowContext(ctx, `
				SELECT c.col, c.due, c.assignee_id, c.assignee_agent, b.archived_at
				FROM cards c JOIN boards b ON b.id = c.board_id
				WHERE c.id = ? AND b.id = ? AND b.space_id = ?`, p.Card, p.Board, sp.ID).
				Scan(&col, &due, &assignee, &agent, &archived)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if archived.Valid || agent || !assignee.Valid || assignee.Int64 != job.UserID ||
				col == string(Done) || col == string(Archived) || !due.Valid || due.String != p.Due {
				return nil
			}
			if notice == notifications.KindCardDue {
				loc, err := auth.UserLocation(ctx, tx, job.UserID)
				if err != nil {
					return err
				}
				day := job.RunAt.In(loc).Format(dateLayout)
				if s.sched.Now().In(loc).Format(dateLayout) > max(day, p.Due) {
					return nil
				}
			}
			return notifications.Add(ctx, tx, notifications.Notice{
				UserID: job.UserID, WorkspaceID: sp.WorkspaceID, Kind: notice,
				Ref:       notifications.Ref{Space: sp.ID, Board: p.Board, Card: p.Card, Due: p.Due},
				DedupeKey: fmt.Sprintf("card:%d:%s", p.Card, p.Due),
			})
		})
	}
}
