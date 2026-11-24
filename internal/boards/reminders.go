package boards

// Due-date reminders: a card that is assigned to a person and has a due date has two jobs on the
// general scheduler (package schedule), both for the assignee. One reminds them on the morning of
// the due day, the other tells them on the morning after that the card is overdue. Each makes one
// notification (package notifications, which says what it is about when it is read).
//
// The jobs follow the card inside the transaction that changes it (syncReminders): a new or changed
// due date or assignee schedules, moves or cancels them, and so does moving the card to done or
// archiving it. Nothing is scheduled for a card whose assignee is the agent, for one nobody is
// assigned to, or for a time that has already passed.
//
// A card's due date is a calendar date with no time zone, and TaskTo knows no person's time zone,
// so "the morning" is reminderHour:00 UTC on the date. That is the same instant for everyone, and
// the morning of the due day in the Americas and Europe; it is early afternoon in east Asia.
//
// A job is only a prompt: when it runs, the handler reads the card again and the assignee's access
// again, and makes nothing if the card is gone, finished, archived, assigned to someone else, due
// on another date than the job was made for, or the assignee may no longer read its space. A
// notification is made with the job's occurrence key, so a retry or a second run of the same
// occurrence makes none twice; a reschedule is a new occurrence.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/schedule"
)

// The kinds of the reminders' jobs.
const (
	KindDueReminder schedule.Kind = "card.due-reminder"
	KindOverdue     schedule.Kind = "card.overdue"
)

// reminderHour is the hour, UTC, of the day a reminder or an overdue notice goes out.
const reminderHour = 8

// reminderPayload is a job's payload: IDs, and the date the job was made for.
type reminderPayload struct {
	Board int64  `json:"board"`
	Card  int64  `json:"card"`
	Due   string `json:"due"`
}

// EnableReminders registers the reminders' handlers on sch, which must not have started, and has
// the service schedule them from then on. A service without it makes none (tests of other parts).
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

// reminderTimes returns when the reminder and the overdue notice of a card due on due go out.
func reminderTimes(due string) (remind, overdue time.Time, err error) {
	day, err := time.Parse(dateLayout, due)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	remind = day.Add(reminderHour * time.Hour)
	return remind, remind.AddDate(0, 0, 1), nil
}

// syncReminders brings the jobs of a card in line with it, in tx, after a change of the card.
// previous is the user the card was assigned to before the change, 0 for none or the agent.
func (s *Service) syncReminders(ctx context.Context, tx *sql.Tx, sp access.Space, boardID, cardID, previous int64) error {
	if s.sched == nil {
		return nil
	}
	var (
		col      string
		due      sql.NullString
		assignee sql.NullInt64
		agent    bool
	)
	if err := tx.QueryRowContext(ctx, `SELECT col, due, assignee_id, assignee_agent FROM cards WHERE id = ?`, cardID).
		Scan(&col, &due, &assignee, &agent); err != nil {
		return err
	}
	var owner int64
	if assignee.Valid && !agent && due.Valid && col != string(Done) && col != string(Archived) {
		owner = assignee.Int64
	}
	// Whoever had them and no longer should, has no reminder.
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
	remind, overdue, err := reminderTimes(due.String)
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
			Kind: j.kind, UserID: owner, WorkspaceID: sp.WorkspaceID, SpaceID: sp.ID,
			DedupeKey: reminderKey(cardID), Payload: payload, RunAt: j.at,
		}
		if !j.at.After(now) {
			// Too late to be a reminder; there is no catching up on a date that has passed.
			if _, err := s.sched.Cancel(ctx, tx, owner, j.kind, job.DedupeKey); err != nil {
				return err
			}
			continue
		}
		switch existing, err := s.sched.Find(ctx, tx, owner, j.kind, job.DedupeKey); {
		case err == nil && existing.RunAt.Equal(j.at) && existing.SpaceID == sp.ID && string(existing.Payload) == string(payload):
			continue // already as it should be; a reschedule would make a new occurrence of one that ran
		case err != nil && !errors.Is(err, schedule.ErrNotFound):
			return err
		}
		if _, err := s.sched.Reschedule(ctx, tx, job); err != nil {
			if errors.Is(err, schedule.ErrTooMany) {
				// A person with a thousand reminders waiting gets no more; the change itself stands.
				slog.Warn("boards: too many scheduled jobs for a reminder", "user", owner)
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

// remind is the handler of both kinds: it notifies the job's owner of the card, if it still holds.
func (s *Service) remind(notice string) schedule.Handler {
	return func(ctx context.Context, job schedule.Job) error {
		var p reminderPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return schedule.Permanent(fmt.Errorf("the payload is malformed: %w", err))
		}
		sp, err := s.access.Check(ctx, job.UserID, job.SpaceID)
		switch {
		case errors.Is(err, access.ErrNotFound):
			return schedule.ErrAccessLost
		case err != nil:
			return err
		case sp.WorkspaceID != job.WorkspaceID || !sp.Access.Allows(access.Read):
			return schedule.ErrAccessLost
		case sp.ArchivedAt != nil:
			return nil // an archived space is left as it is
		}
		return s.write(ctx, func(tx *sql.Tx) error {
			var (
				col      string
				due      sql.NullString
				assignee sql.NullInt64
				agent    bool
				archived sql.NullString
			)
			err := tx.QueryRowContext(ctx, `
				SELECT c.col, c.due, c.assignee_id, c.assignee_agent, b.archived_at
				FROM cards c JOIN boards b ON b.id = c.board_id
				WHERE c.id = ? AND b.id = ? AND b.space_id = ?`, p.Card, p.Board, sp.ID).
				Scan(&col, &due, &assignee, &agent, &archived)
			if errors.Is(err, sql.ErrNoRows) {
				return nil // the card is gone
			}
			if err != nil {
				return err
			}
			if archived.Valid || agent || !assignee.Valid || assignee.Int64 != job.UserID ||
				col == string(Done) || col == string(Archived) || !due.Valid || due.String != p.Due {
				return nil // not what the job was made for
			}
			if notice == notifications.KindCardDue && s.sched.Now().UTC().Format(dateLayout) > p.Due {
				return nil // the day passed (the server was away): the overdue notice is the one that is true
			}
			return notifications.Add(ctx, tx, notifications.Notice{
				UserID: job.UserID, WorkspaceID: sp.WorkspaceID, Kind: notice,
				Ref:       notifications.Ref{Space: sp.ID, Board: p.Board, Card: p.Card},
				DedupeKey: job.Occurrence(),
			})
		})
	}
}
