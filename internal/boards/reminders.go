package boards

// Due-date reminders: a card that is assigned to a person and has a due date has two jobs on the
// general scheduler (package schedule), both for the assignee. One reminds them on the morning of
// the due day, the other tells them on the morning after that the card is overdue. Each makes one
// notification (package notifications, which says what it is about when it is read).
//
// The jobs follow the card inside the transaction that changes it (syncReminders): a new or changed
// due date or assignee schedules, moves or cancels them, and so does moving the card to done or
// archiving it, archiving its board or its space (and bringing them back), and the assignee losing
// access to the space (the schema drops their jobs with it) and getting it back (SyncSpaceMember and
// SyncWorkspaceMember, hooks of the transaction that adds them). Nothing is scheduled for a card whose
// assignee is the agent, for one nobody is assigned to, for an assignee who may not read the space,
// or for a time that has already passed or that the scheduler can't hold (the card's date is
// still saved: a reminder never fails a change to a card). A server that starts with cards that
// were dated before reminders existed schedules theirs (ReconcileReminders).
//
// A card's due date is a calendar date with no time zone, so "the morning" is the assignee's:
// reminderHour:00 on the date in their time zone (package auth, timezone.go; UTC for a person who has
// none yet). The time is computed from the calendar date and the zone, never as a number of hours
// from midnight UTC, so it stays 08:00 on the wall clock across daylight saving time, and an hour
// that a change of clocks skips is the first moment after the gap. A change of the person's zone
// moves their jobs (SyncTimeZone), and so does giving the card to someone else, whose zone is the
// one used.
//
// A job is only a prompt: when it runs, the handler reads the card again and the assignee's access
// again, in the transaction that makes the notification, and makes nothing if the job is no longer
// the live claim, the card is gone, finished, archived, assigned to someone else, due on another
// date than the job was made for, or the assignee may no longer read its space. A notification is
// made once for a person, a card, a kind and a due date, whatever happens to the jobs: a retry, a
// reschedule or a clock that stepped back make none twice.

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

// The kinds of the reminders' jobs.
const (
	KindDueReminder schedule.Kind = "card.due-reminder"
	KindOverdue     schedule.Kind = "card.overdue"
)

// reminderHour is the hour, in the assignee's time zone, of the day a reminder or an overdue notice
// goes out.
const reminderHour = 8

// reminderPayload is a job's payload: IDs, and the date the job was made for.
type reminderPayload struct {
	Board int64  `json:"board"`
	Card  int64  `json:"card"`
	Due   string `json:"due"`
}

// WithScheduler has the service schedule reminders on sch, whose handlers another service of the
// same database registered (EnableReminders): the HTTP routes and the agent's tools must both
// keep the jobs, and a kind has one handler. sch is set before the service takes requests.
func (s *Service) WithScheduler(sch *schedule.Service) *Service {
	s.sched = sch
	return s
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

// reminderTimes returns when the reminder and the overdue notice of a card due on due go out for
// someone in loc: reminderHour o'clock there on the due date and on the date after it.
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

// syncReminders brings the jobs of a card in line with it, in tx, after a change of the card or of
// what the card is in. previous is the user the card was assigned to before the change, 0 for none
// or the agent. It fails only for an error of the database: a reminder that can't be made (too
// many waiting, a time out of the scheduler's range) is left out and the change stands.
func (s *Service) syncReminders(ctx context.Context, tx *sql.Tx, cardID, previous int64) error {
	return s.syncCard(ctx, tx, cardID, previous, nil)
}

// reminderCap remembers, through one walk over many cards, whose reminders the scheduler refused
// for having too many waiting, so the walk can stop. A nil reminderCap remembers nothing.
type reminderCap map[int64]map[schedule.Kind]bool

func (c reminderCap) mark(user int64, kind schedule.Kind) {
	if c[user] == nil {
		c[user] = map[schedule.Kind]bool{}
	}
	c[user][kind] = true
}

func (c reminderCap) has(user int64, kind schedule.Kind) bool { return c[user][kind] }

// both is whether neither kind of reminder can be added for user any more.
func (c reminderCap) both(user int64) bool {
	return c.has(user, KindDueReminder) && c.has(user, KindOverdue)
}

// syncCard is syncReminders that notes in capped (when not nil) the people the scheduler has no
// room left for.
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
		// Only for someone who may read the space: a job of anyone else would fail when it came
		// due, and hold a place of their bound until then.
		switch _, err := s.access.CheckTx(ctx, tx, assignee.Int64, spaceID); {
		case err == nil:
			owner = assignee.Int64
		case !errors.Is(err, access.ErrNotFound):
			return err
		}
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
		// A job that is as it should be stays, whatever its time and state: one that is ready but has
		// not run yet (the worker is behind, or the server was away) is not for a change of the card's
		// labels to take away, and one that ran must not run again as a new occurrence.
		switch existing, err := s.sched.Find(ctx, tx, owner, j.kind, job.DedupeKey); {
		case err == nil && existing.RunAt.Equal(j.at) && existing.SpaceID == spaceID && string(existing.Payload) == string(payload):
			continue
		case err != nil && !errors.Is(err, schedule.ErrNotFound):
			return err
		}
		// Each time is held to the scheduler's range by itself: the day after the last supported day
		// has no overdue notice, and still has its reminder.
		if !j.at.After(now) || j.at.After(schedule.MaxRunAt) {
			// Too late to be a reminder; there is no catching up on a date that has passed.
			if _, err := s.sched.Cancel(ctx, tx, owner, j.kind, job.DedupeKey); err != nil {
				return err
			}
			continue
		}
		if _, err := s.sched.Reschedule(ctx, tx, job); err != nil {
			if errors.Is(err, schedule.ErrTooMany) || errors.Is(err, schedule.ErrInvalid) {
				// A person with a thousand reminders waiting gets no more; the change itself stands.
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

// syncCards brings the jobs of the cards the query selects (their IDs, one column) in line with
// them, in tx. For a walk over the cards of one person (forUser, not 0) it stops once the
// scheduler has refused both kinds of their reminders for having too many waiting, rather than
// walk on with the membership change's write lock held: the change stands, as for a card's own.
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

// dated is the cards of a board or space that can have reminders: assigned to a person, with a date.
const dated = `c.assignee_id IS NOT NULL AND NOT c.assignee_agent AND c.due IS NOT NULL`

// open is the cards a reminder can still be for: not finished or archived.
const open = `c.col NOT IN ('done', 'archived')`

// syncBoard follows a board that was archived or brought back: its cards' jobs go or come.
func (s *Service) syncBoard(ctx context.Context, tx *sql.Tx, boardID int64) error {
	return s.syncCards(ctx, tx, 0, `SELECT c.id FROM cards c WHERE c.board_id = ? AND `+dated+` ORDER BY c.id`, boardID)
}

// SyncSpace follows a shared space that was archived or brought back: the jobs of its cards go or
// come. It is the access package's ArchiveHook (access.Checker.OnArchiveChange).
func (s *Service) SyncSpace(ctx context.Context, tx *sql.Tx, spaceID int64) error {
	return s.syncCards(ctx, tx, 0, `
		SELECT c.id FROM cards c JOIN boards b ON b.id = c.board_id
		WHERE b.space_id = ? AND `+dated+` ORDER BY c.id`, spaceID)
}

// SyncSpaceMember follows a person who was added to a shared space: the jobs of the cards in it that
// are assigned to them come back (the schema dropped them when they left). It is the access
// package's MemberHook (access.Checker.OnMemberAdded).
func (s *Service) SyncSpaceMember(ctx context.Context, tx *sql.Tx, spaceID, userID int64) error {
	return s.syncCards(ctx, tx, userID, `
		SELECT c.id FROM cards c JOIN boards b ON b.id = c.board_id
		WHERE b.space_id = ? AND c.assignee_id = ? AND `+dated+` AND `+open+` ORDER BY c.id`, spaceID, userID)
}

// SyncWorkspaceMember follows a person who was added to a workspace: the jobs of the cards assigned
// to them in the workspace's spaces come back. A shared space has none to find until they are added
// to it again (SyncSpaceMember), since they are no member of it now; their private space has. It is
// the auth package's MemberHook (auth.Service.OnMemberAdded).
func (s *Service) SyncWorkspaceMember(ctx context.Context, tx *sql.Tx, workspaceID, userID int64) error {
	return s.syncCards(ctx, tx, userID, `
		SELECT c.id FROM cards c JOIN boards b ON b.id = c.board_id JOIN spaces sp ON sp.id = b.space_id
		WHERE sp.workspace_id = ? AND c.assignee_id = ? AND `+dated+` AND `+open+` ORDER BY c.id`, workspaceID, userID)
}

// SyncTimeZone follows a person whose time zone changed: the jobs of the open cards assigned to
// them are timed again, by the new zone (a job that is as it should be is left alone). It is the
// auth package's TimeZoneHook (auth.Service.OnTimeZoneChanged).
func (s *Service) SyncTimeZone(ctx context.Context, tx *sql.Tx, userID int64) error {
	return s.syncCards(ctx, tx, userID, `
		SELECT c.id FROM cards c WHERE c.assignee_id = ? AND `+dated+` AND `+open+` ORDER BY c.id`, userID)
}

// reconcileBatch is how many cards ReconcileReminders brings in line in one transaction.
const reconcileBatch = 200

// ReconcileReminders schedules the reminders of the cards that should have them and have none:
// the cards dated before reminders existed, and those a restore or an access change left without.
// It is idempotent (a card whose jobs are as they should be is not touched), and the server runs
// it at start, before the scheduler's worker does.
func (s *Service) ReconcileReminders(ctx context.Context) error {
	if s.sched == nil {
		return nil
	}
	// A card due yesterday still has its overdue notice ahead of it, and in the west of the world the
	// morning after a date is up to 20 hours past its end in UTC: two days back covers every zone.
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

// remind is the handler of both kinds: it notifies the job's owner of the card, if it still holds.
func (s *Service) remind(notice string) schedule.Handler {
	return func(ctx context.Context, job schedule.Job) error {
		var p reminderPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return schedule.Permanent(fmt.Errorf("the payload is malformed: %w", err))
		}
		// Everything is checked in the transaction that makes the notification, which holds the write
		// lock: what was true a moment ago (the claim, the access, the card) is true when it commits.
		return s.write(ctx, func(tx *sql.Tx) error {
			if holds, err := s.sched.Holds(ctx, tx, job); err != nil || !holds {
				return err // cancelled or rescheduled since this try began: what replaced it speaks
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
				return nil // an archived space is left as it is
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
				return nil // the card is gone
			}
			if err != nil {
				return err
			}
			if archived.Valid || agent || !assignee.Valid || assignee.Int64 != job.UserID ||
				col == string(Done) || col == string(Archived) || !due.Valid || due.String != p.Due {
				return nil // not what the job was made for
			}
			if notice == notifications.KindCardDue {
				// The day is the person's own: the job was timed by their zone, which this transaction
				// holds still (a change of it moves the jobs in its own commit).
				loc, err := auth.UserLocation(ctx, tx, job.UserID)
				if err != nil {
					return err
				}
				if s.sched.Now().In(loc).Format(dateLayout) > p.Due {
					return nil // the day passed (the server was away): the overdue notice is the one that is true
				}
			}
			return notifications.Add(ctx, tx, notifications.Notice{
				UserID: job.UserID, WorkspaceID: sp.WorkspaceID, Kind: notice,
				Ref: notifications.Ref{Space: sp.ID, Board: p.Board, Card: p.Card, Due: p.Due},
				// One for the person, the card, the kind (the notification's own) and the date, however
				// many jobs there were: Alice, then Bob, then Alice again, or a clock that went back.
				DedupeKey: fmt.Sprintf("card:%d:%s", p.Card, p.Due),
			})
		})
	}
}
