package apps

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/schedule"
)

const KindRecordReminder schedule.Kind = "app.record-reminder"

const RemindersCapability = "app.reminders"

const (
	remindersVersion    = 3
	maxReminders        = 3
	defaultReminderAt   = "09:00"
	maxReminderTitleLen = 200
)

type Reminder struct {
	Name  string `json:"name"`
	Date  string `json:"date"`
	Time  string `json:"time,omitempty"`
	At    string `json:"at,omitempty"`
	Title string `json:"title"`
	Where *Rule  `json:"where,omitempty"`
}

func (r *Reminder) UnmarshalJSON(b []byte) error {
	type plain Reminder
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p plain
	if err := dec.Decode(&p); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("a reminder is one JSON object")
	}
	*r = Reminder(p)
	return nil
}

func (d Definition) checkReminders(c Collection) (bool, error) {
	if len(d.Reminders) == 0 {
		return false, nil
	}
	if len(d.Reminders) > maxReminders {
		return false, invalidf("reminders: at most %d", maxReminders)
	}
	seen := map[string]bool{}
	for i, r := range d.Reminders {
		if !namePattern.MatchString(r.Name) {
			return false, invalidf("reminders: %d: a name is a lowercase letter, then up to 39 lowercase letters, digits or _", i)
		}
		if seen[r.Name] {
			return false, invalidf("reminders: %q is declared twice", r.Name)
		}
		seen[r.Name] = true
		if err := r.check(c); err != nil {
			return false, invalidf("reminders: %q: %v", r.Name, err)
		}
	}
	return true, nil
}

func (r Reminder) check(c Collection) error {
	date, ok := c.field(r.Date)
	if !ok || date.Type != TypeDate {
		return fmt.Errorf("date names %q, which is not a date field of the collection", r.Date)
	}
	if r.Time != "" {
		if clock, ok := c.field(r.Time); !ok || clock.Type != TypeTime {
			return fmt.Errorf("time names %q, which is not a time field of the collection", r.Time)
		}
	}
	if r.At != "" && !timePattern.MatchString(r.At) {
		return fmt.Errorf("at is a time of day, HH:MM")
	}
	if r.Title == "" {
		return fmt.Errorf("a reminder needs a title")
	}
	if err := checkTemplate("title", r.Title, c, maxReminderTitleLen); err != nil {
		return err
	}
	if r.Where != nil {
		if err := r.Where.check(c); err != nil {
			return fmt.Errorf("where: %w", err)
		}
	}
	return nil
}

func (d Definition) reminder(name string) (Reminder, bool) {
	for _, r := range d.Reminders {
		if r.Name == name {
			return r, true
		}
	}
	return Reminder{}, false
}

func (r Reminder) applies(c Collection, data map[string]any) bool {
	return r.Where == nil || c.holds(*r.Where, data)
}

func (r Reminder) runAt(c Collection, data map[string]any, loc *time.Location) (time.Time, bool) {
	date, ok := c.field(r.Date)
	if !ok {
		return time.Time{}, false
	}
	day, ok := storedText(date, data)
	if !ok {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return time.Time{}, false
	}
	clock := defaultReminderAt
	if r.At != "" {
		clock = r.At
	}
	if r.Time != "" {
		if field, ok := c.field(r.Time); ok {
			if text, ok := storedText(field, data); ok && timePattern.MatchString(text) {
				clock = text
			}
		}
	}
	hour, _ := strconv.Atoi(clock[:2])
	minute, _ := strconv.Atoi(clock[3:])
	y, m, d := parsed.Date()
	return schedule.LocalHour(y, m, d, hour, loc).Add(time.Duration(minute) * time.Minute).UTC(), true
}

func storedText(f Field, data map[string]any) (string, bool) {
	value, set := data[f.Name]
	if !set {
		return "", false
	}
	v, present, err := f.stored(value)
	text, isText := v.(string)
	return text, err == nil && present && isText
}

type reminderPayload struct {
	App  int64  `json:"app"`
	Row  int64  `json:"row"`
	Name string `json:"name"`
}

func reminderKey(appID, rowID int64, name string) string {
	return fmt.Sprintf("app:%d:row:%d:%s", appID, rowID, name)
}

func (s *Service) WithScheduler(sch *schedule.Service) *Service {
	s.sched = sch
	return s
}

func (s *Service) EnableReminders(sch *schedule.Service) error {
	if err := sch.Register(KindRecordReminder, s.remindRecord); err != nil {
		return err
	}
	s.sched = sch
	return nil
}

type reminderScope struct {
	appID, spaceID, workspaceID int64
	def                         Definition
	dormant                     bool
	locations                   map[int64]*time.Location
	readers                     map[int64]bool
}

func (s *Service) reminderScopeOf(ctx context.Context, tx *sql.Tx, appID int64) (*reminderScope, bool, error) {
	sc := &reminderScope{appID: appID, locations: map[int64]*time.Location{}, readers: map[int64]bool{}}
	var spaceArchived sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT a.space_id, sp.workspace_id, sp.archived_at FROM apps a JOIN spaces sp ON sp.id = a.space_id WHERE a.id = ?`, appID).
		Scan(&sc.spaceID, &sc.workspaceID, &spaceArchived)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	a, err := app(ctx, tx, sc.spaceID, appID)
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if a.liveDef == nil || len(a.liveDef.Reminders) == 0 {
		return nil, false, nil
	}
	sc.def = *a.liveDef
	sc.dormant = a.ArchivedAt != nil || spaceArchived.Valid
	return sc, true, nil
}

func (sc *reminderScope) location(ctx context.Context, tx *sql.Tx, userID int64) (*time.Location, error) {
	if loc, ok := sc.locations[userID]; ok {
		return loc, nil
	}
	loc, err := auth.UserLocation(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	sc.locations[userID] = loc
	return loc, nil
}

func (s *Service) mayBeReminded(ctx context.Context, tx *sql.Tx, sc *reminderScope, userID int64) (bool, error) {
	if may, ok := sc.readers[userID]; ok {
		return may, nil
	}
	acc, err := s.access.On(tx).App(ctx, userID, sc.spaceID, sc.appID)
	may := err == nil && acc.View && acc.ReadRecords != access.ScopeNone
	if err != nil && !errors.Is(err, access.ErrNotFound) && !errors.Is(err, access.ErrAppNotFound) && !errors.Is(err, access.ErrDisabled) {
		return false, err
	}
	sc.readers[userID] = may
	return may, nil
}

func (s *Service) syncRecordReminders(ctx context.Context, tx *sql.Tx, appID, rowID int64) error {
	if s.sched == nil {
		return nil
	}
	sc, found, err := s.reminderScopeOf(ctx, tx, appID)
	if err != nil || !found {
		return err
	}
	return s.syncRowReminders(ctx, tx, sc, rowID)
}

func (s *Service) syncAppReminders(ctx context.Context, tx *sql.Tx, appID, forUser int64) error {
	if s.sched == nil {
		return nil
	}
	sc, found, err := s.reminderScopeOf(ctx, tx, appID)
	if err != nil || !found {
		return err
	}
	query := `SELECT id FROM app_rows WHERE app_id = ? AND deleted_at IS NULL AND created_by IS NOT NULL`
	args := []any{appID}
	if forUser != 0 {
		query += ` AND created_by = ?`
		args = append(args, forUser)
	}
	ids, err := scanIDs(ctx, tx, query+` ORDER BY id`, args...)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.syncRowReminders(ctx, tx, sc, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) syncSpaceReminders(ctx context.Context, tx *sql.Tx, spaceID, forUser int64) error {
	if s.sched == nil {
		return nil
	}
	ids, err := scanIDs(ctx, tx, `SELECT id FROM apps WHERE space_id = ? ORDER BY id`, spaceID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.syncAppReminders(ctx, tx, id, forUser); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) SyncSpaceReminders(ctx context.Context, tx *sql.Tx, spaceID int64) error {
	return s.syncSpaceReminders(ctx, tx, spaceID, 0)
}

func (s *Service) SyncSpaceMemberReminders(ctx context.Context, tx *sql.Tx, spaceID, userID int64) error {
	return s.syncSpaceReminders(ctx, tx, spaceID, userID)
}

func (s *Service) SyncWorkspaceMemberReminders(ctx context.Context, tx *sql.Tx, workspaceID, userID int64) error {
	if s.sched == nil {
		return nil
	}
	return s.syncUserApps(ctx, tx, `
		SELECT DISTINCT r.app_id FROM app_rows r JOIN apps a ON a.id = r.app_id JOIN spaces sp ON sp.id = a.space_id
		WHERE sp.workspace_id = ? AND r.created_by = ? AND r.deleted_at IS NULL ORDER BY r.app_id`, userID, workspaceID, userID)
}

func (s *Service) SyncTimeZoneReminders(ctx context.Context, tx *sql.Tx, userID int64) error {
	if s.sched == nil {
		return nil
	}
	return s.syncUserApps(ctx, tx, `
		SELECT DISTINCT app_id FROM app_rows WHERE created_by = ? AND deleted_at IS NULL ORDER BY app_id`, userID, userID)
}

func (s *Service) syncUserApps(ctx context.Context, tx *sql.Tx, query string, userID int64, args ...any) error {
	ids, err := scanIDs(ctx, tx, query, args...)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.syncAppReminders(ctx, tx, id, userID); err != nil {
			return err
		}
	}
	return nil
}

func scanIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, errors.Join(rows.Err(), rows.Close())
}

func (s *Service) syncRowReminders(ctx context.Context, tx *sql.Tx, sc *reminderScope, rowID int64) error {
	var (
		raw     string
		creator sql.NullInt64
		deleted sql.NullString
	)
	err := tx.QueryRowContext(ctx, `SELECT data, created_by, deleted_at FROM app_rows WHERE id = ? AND app_id = ?`, rowID, sc.appID).
		Scan(&raw, &creator, &deleted)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !creator.Valid {
		return nil
	}
	if err != nil {
		return err
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return fmt.Errorf("app %d row %d: %w", sc.appID, rowID, err)
	}
	owner := creator.Int64
	live := !deleted.Valid && !sc.dormant
	if live {
		if live, err = s.mayBeReminded(ctx, tx, sc, owner); err != nil {
			return err
		}
	}
	var loc *time.Location
	now := s.sched.Now()
	for _, r := range sc.def.Reminders {
		key := reminderKey(sc.appID, rowID, r.Name)
		var at time.Time
		due := false
		if live && r.applies(sc.def.Collection, data) {
			if loc == nil {
				if loc, err = sc.location(ctx, tx, owner); err != nil {
					return err
				}
			}
			at, due = r.runAt(sc.def.Collection, data, loc)
		}
		payload, err := json.Marshal(reminderPayload{App: sc.appID, Row: rowID, Name: r.Name})
		if err != nil {
			return err
		}
		job := schedule.Job{
			Kind: KindRecordReminder, UserID: owner, WorkspaceID: sc.workspaceID, SpaceID: sc.spaceID,
			DedupeKey: key, Payload: payload, RunAt: at,
		}
		keptPending := false
		if due {
			switch existing, err := s.sched.Find(ctx, tx, owner, KindRecordReminder, key); {
			case err == nil && existing.RunAt.Equal(at) && string(existing.Payload) == string(payload):
				if existing.SpaceID == sc.spaceID || existing.State != schedule.StatePending {
					continue
				}
				keptPending = true
			case err != nil && !errors.Is(err, schedule.ErrNotFound):
				return err
			}
		}
		if !due || !keptPending && !at.After(now) || at.After(schedule.MaxRunAt) || at.Before(schedule.MinRunAt) {
			if _, err := s.sched.Cancel(ctx, tx, owner, KindRecordReminder, key); err != nil {
				return err
			}
			continue
		}
		if _, err := s.sched.Reschedule(ctx, tx, job); err != nil {
			if errors.Is(err, schedule.ErrTooMany) || errors.Is(err, schedule.ErrInvalid) {
				slog.Warn("apps: a reminder was not scheduled", "app", sc.appID, "user", owner, "err", err)
				continue
			}
			return err
		}
	}
	return nil
}

func (s *Service) remindRecord(ctx context.Context, job schedule.Job) error {
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
		case errors.Is(err, access.ErrDisabled):
			return schedule.ErrOwnerDisabled
		case errors.Is(err, access.ErrNotFound):
			return schedule.ErrAccessLost
		case err != nil:
			return err
		case sp.WorkspaceID != job.WorkspaceID:
			return schedule.ErrAccessLost
		case sp.ArchivedAt != nil:
			return nil
		}
		a, err := app(ctx, tx, sp.ID, p.App)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if a.ArchivedAt != nil || a.liveDef == nil {
			return nil
		}
		r, ok := a.liveDef.reminder(p.Name)
		if !ok {
			return nil
		}
		acc, err := s.access.On(tx).App(ctx, job.UserID, sp.ID, p.App)
		switch {
		case errors.Is(err, access.ErrNotFound), errors.Is(err, access.ErrAppNotFound):
			return schedule.ErrAccessLost
		case errors.Is(err, access.ErrDisabled):
			return schedule.ErrOwnerDisabled
		case err != nil:
			return err
		case !acc.View || acc.ReadRecords == access.ScopeNone:
			return schedule.ErrAccessLost
		}
		var (
			raw     string
			creator sql.NullInt64
		)
		err = tx.QueryRowContext(ctx, `SELECT data, created_by FROM app_rows WHERE id = ? AND app_id = ? AND deleted_at IS NULL`, p.Row, p.App).
			Scan(&raw, &creator)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !creator.Valid || creator.Int64 != job.UserID || !readable(acc, job.UserID).sees(creator) {
			return nil
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return fmt.Errorf("app %d row %d: %w", p.App, p.Row, err)
		}
		if !r.applies(a.liveDef.Collection, data) {
			return nil
		}
		loc, err := auth.UserLocation(ctx, tx, job.UserID)
		if err != nil {
			return err
		}
		at, due := r.runAt(a.liveDef.Collection, data, loc)
		if !due {
			return nil
		}
		if !at.Equal(job.RunAt) && at.After(s.sched.Now()) {
			moved := job
			moved.SpaceID, moved.RunAt = sp.ID, at
			if _, err := s.sched.Reschedule(ctx, tx, moved); err != nil {
				if errors.Is(err, schedule.ErrTooMany) || errors.Is(err, schedule.ErrInvalid) {
					return nil
				}
				return err
			}
			return nil
		}
		return notifications.Add(ctx, tx, notifications.Notice{
			UserID: job.UserID, WorkspaceID: sp.WorkspaceID, Kind: notifications.KindAppReminder,
			Ref:       notifications.Ref{Space: sp.ID, App: p.App, Record: p.Row, Reminder: p.Name},
			DedupeKey: job.Occurrence(),
		})
	})
}

func (s *Service) ReminderText(ctx context.Context, c *access.Checker, userID, workspaceID int64, ref notifications.Ref) (notifications.ReminderText, bool, error) {
	var out notifications.ReminderText
	shown := false
	err := c.Snapshot(ctx, func(snap *access.Checker, tx *sql.Tx) error {
		sp, err := snap.Check(ctx, userID, ref.Space)
		if errors.Is(err, access.ErrNotFound) || err == nil && (sp.WorkspaceID != workspaceID || sp.ArchivedAt != nil) {
			return nil
		}
		if err != nil {
			return err
		}
		acc, err := snap.App(ctx, userID, sp.ID, ref.App)
		if errors.Is(err, access.ErrNotFound) || errors.Is(err, access.ErrAppNotFound) ||
			err == nil && (!acc.View || acc.ReadRecords == access.ScopeNone) {
			return nil
		}
		if err != nil {
			return err
		}
		a, err := app(ctx, tx, sp.ID, ref.App)
		if errors.Is(err, ErrNotFound) || err == nil && (a.ArchivedAt != nil || a.liveDef == nil) {
			return nil
		}
		if err != nil {
			return err
		}
		r, ok := a.liveDef.reminder(ref.Reminder)
		if !ok {
			return nil
		}
		var (
			raw     string
			creator sql.NullInt64
		)
		err = tx.QueryRowContext(ctx, `SELECT data, created_by FROM app_rows WHERE id = ? AND app_id = ? AND deleted_at IS NULL`, ref.Record, ref.App).
			Scan(&raw, &creator)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !readable(acc, userID).sees(creator) || !creator.Valid || creator.Int64 != userID {
			return nil
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return fmt.Errorf("app %d row %d: %w", ref.App, ref.Record, err)
		}
		if !r.applies(a.liveDef.Collection, data) {
			return nil
		}
		title := render(r.Title, a.liveDef.Collection, data, ref.Record, modeLine)
		if title == "" {
			title = a.Name
		}
		out, shown = notifications.ReminderText{App: a.Name, Title: title}, true
		return nil
	})
	return out, shown, err
}
