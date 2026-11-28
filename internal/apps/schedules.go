package apps

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/schedule"
)

const (
	KindScheduledAction schedule.Kind = "app.scheduled-action"

	SchedulesCapability = "app.schedules"
	schedulesVersion    = 4
	maxSchedules        = 5
	probeDay            = "2000-01-01"
	todayLayout         = "2006-01-02"
)

const (
	scheduleErrAccess  = "access"
	scheduleErrInvalid = "invalid"
	scheduleErrFull    = "full"
	scheduleErrLimit   = "limit"
)

type Schedule struct {
	Action      string            `json:"action"`
	Calendar    schedule.Calendar `json:"calendar"`
	Values      map[string]any    `json:"values,omitempty" apitypes:"json"`
	TodayFields []string          `json:"today_fields,omitempty"`
}

type schedulePayload struct {
	App  int64  `json:"app"`
	Name string `json:"name"`
}

func (d Definition) checkSchedules(out Definition) (map[string]Schedule, error) {
	if len(d.Schedules) == 0 {
		return nil, nil
	}
	if len(d.Schedules) > maxSchedules {
		return nil, invalidf("schedules: at most %d", maxSchedules)
	}
	checked := make(map[string]Schedule, len(d.Schedules))
	for name, sch := range d.Schedules {
		if !namePattern.MatchString(name) {
			return nil, invalidf("schedules: %q: a name is a lowercase letter, then up to 39 lowercase letters, digits or _", name)
		}
		if act, ok := d.Actions[sch.Action]; !ok || act.Kind != ActionInsert {
			return nil, invalidf("schedules: %q: action must name an insert action", name)
		}
		cal, err := sch.Calendar.Normal()
		if err != nil {
			return nil, invalidf("schedules: %q: calendar: %v", name, err)
		}
		sch.Calendar = cal
		planned := map[string]any{}
		for field, v := range sch.Values {
			f, ok := out.Collection.field(field)
			if !ok {
				return nil, invalidf("schedules: %q: values name %q, which is not a field", name, field)
			}
			if !scheduledType(f.Type) {
				return nil, invalidf("schedules: %q: %q is a %s: only a person can capture one", name, field, f.Type)
			}
			planned[field] = v
		}
		for _, field := range sch.TodayFields {
			f, ok := out.Collection.field(field)
			if !ok || f.Type != TypeDate {
				return nil, invalidf("schedules: %q: today_fields name date fields, and %q is not one", name, field)
			}
			if _, twice := planned[field]; twice {
				return nil, invalidf("schedules: %q: %q is set twice", name, field)
			}
			planned[field] = probeDay
		}
		if _, err := Plan(SurfaceID(0), out, ClientAction{Name: sch.Action, SurfaceID: SurfaceID(0), Context: planned}); err != nil {
			return nil, invalidf("schedules: %q: the record it inserts isn't valid: %v", name, err)
		}
		checked[name] = sch
	}
	return checked, nil
}

func scheduledType(t string) bool {
	return t != TypeAttachment && t != TypeSignature && t != TypeLocation
}

func (s *Service) WithScheduler(sch *schedule.Service) *Service {
	s.sched = sch
	return s
}

func (s *Service) EnableSchedules(sch *schedule.Service) error {
	if err := sch.Register(KindScheduledAction, s.runScheduled); err != nil {
		return err
	}
	s.sched = sch
	return nil
}

func scheduleKey(appID int64, name string) string { return fmt.Sprintf("app:%d:%s", appID, name) }

type storedSchedule struct {
	runner   sql.NullInt64
	calendar string
}

func storedSchedules(ctx context.Context, tx *sql.Tx, appID int64) (map[string]storedSchedule, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name, runner_id, calendar FROM app_schedules WHERE app_id = ?`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]storedSchedule{}
	for rows.Next() {
		var name string
		var st storedSchedule
		if err := rows.Scan(&name, &st.runner, &st.calendar); err != nil {
			return nil, err
		}
		out[name] = st
	}
	return out, rows.Err()
}

func (s *Service) syncSchedules(ctx context.Context, tx *sql.Tx, appID, runner int64) error {
	if s.sched == nil {
		return nil
	}
	var spaceID, workspaceID int64
	var archived sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT a.space_id, sp.workspace_id, a.archived_at FROM apps a JOIN spaces sp ON sp.id = a.space_id WHERE a.id = ?`,
		appID).Scan(&spaceID, &workspaceID, &archived); err != nil {
		return err
	}
	cur, err := app(ctx, tx, spaceID, appID)
	if err != nil {
		return err
	}
	var want map[string]Schedule
	if cur.liveDef != nil {
		want = cur.liveDef.Schedules
	}
	stored, err := storedSchedules(ctx, tx, appID)
	if err != nil {
		return err
	}
	for name := range stored {
		if _, kept := want[name]; !kept {
			if _, err := tx.ExecContext(ctx, `DELETE FROM app_schedules WHERE app_id = ? AND name = ?`, appID, name); err != nil {
				return err
			}
		}
	}
	for name, sch := range want {
		old, had := stored[name]
		who := runner
		if who == 0 {
			who = old.runner.Int64
		}
		calendar, err := json.Marshal(sch.Calendar)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO app_schedules (app_id, name, runner_id, calendar) VALUES (?, ?, nullif(?, 0), ?)
			ON CONFLICT (app_id, name) DO UPDATE SET runner_id = excluded.runner_id, calendar = excluded.calendar`,
			appID, name, who, string(calendar)); err != nil {
			return err
		}
		if had && old.runner.Valid && old.runner.Int64 != who {
			if _, err := s.sched.Cancel(ctx, tx, old.runner.Int64, KindScheduledAction, scheduleKey(appID, name)); err != nil {
				return err
			}
		}
		plan := scheduleSlot{app: appID, name: name, runner: who, space: spaceID, workspace: workspaceID, archived: archived.Valid,
			unchanged: had && old.runner.Int64 == who && old.calendar == string(calendar)}
		if err := s.planSchedule(ctx, tx, plan, sch); err != nil {
			return err
		}
	}
	return nil
}

type scheduleSlot struct {
	app, runner, space, workspace int64
	name                          string
	archived, unchanged           bool
}

func (s *Service) planSchedule(ctx context.Context, tx *sql.Tx, p scheduleSlot, sch Schedule) error {
	key := scheduleKey(p.app, p.name)
	if p.archived {
		_, err := s.sched.Cancel(ctx, tx, p.runner, KindScheduledAction, key)
		return err
	}
	if _, err := s.access.CheckTx(ctx, tx, p.runner, p.space); err != nil {
		if !errors.Is(err, access.ErrNotFound) {
			return err
		}
		_, err := s.sched.Cancel(ctx, tx, p.runner, KindScheduledAction, key)
		return err
	}
	if p.unchanged {
		if existing, err := s.sched.Find(ctx, tx, p.runner, KindScheduledAction, key); err == nil &&
			existing.SpaceID == p.space && (existing.State == schedule.StatePending || existing.State == schedule.StateRunning) {
			return nil
		} else if err != nil && !errors.Is(err, schedule.ErrNotFound) {
			return err
		}
	}
	payload, err := json.Marshal(schedulePayload{App: p.app, Name: p.name})
	if err != nil {
		return err
	}
	next, err := sch.Calendar.Next(s.sched.Now())
	if err != nil {
		return err
	}
	_, err = s.sched.Reschedule(ctx, tx, schedule.Job{
		Kind: KindScheduledAction, UserID: p.runner, WorkspaceID: p.workspace, SpaceID: p.space,
		DedupeKey: key, Payload: payload, RunAt: next,
	})
	if errors.Is(err, schedule.ErrTooMany) {
		slog.Warn("apps: a schedule was not planned", "app", p.app, "user", p.runner, "err", err)
		return s.noteSchedule(ctx, tx, p.app, p.name, scheduleErrLimit, false)
	}
	return err
}

func (s *Service) noteSchedule(ctx context.Context, tx *sql.Tx, appID int64, name, code string, ran bool) error {
	var at any
	if ran {
		at = now()
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE app_schedules SET last_error = nullif(?, ''), last_run_at = coalesce(?, last_run_at) WHERE app_id = ? AND name = ?`,
		code, at, appID, name)
	return err
}

func scheduledOperation(job schedule.Job) string {
	sum := sha256.Sum256([]byte(job.Occurrence()))
	return "sched-" + hex.EncodeToString(sum[:])[:32]
}

func scheduledContext(sch Schedule, at time.Time) (map[string]any, error) {
	loc, err := time.LoadLocation(sch.Calendar.Zone)
	if err != nil {
		return nil, err
	}
	planned := make(map[string]any, len(sch.Values)+len(sch.TodayFields))
	for k, v := range sch.Values {
		planned[k] = v
	}
	for _, field := range sch.TodayFields {
		planned[field] = at.In(loc).Format(todayLayout)
	}
	return planned, nil
}

func (s *Service) runScheduled(ctx context.Context, job schedule.Job) error {
	var p schedulePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return schedule.Permanent(err)
	}
	var runner sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT runner_id FROM app_schedules WHERE app_id = ? AND name = ?`, p.App, p.Name).Scan(&runner)
	if errors.Is(err, sql.ErrNoRows) || err == nil && runner.Int64 != job.UserID {
		return nil
	}
	if err != nil {
		return err
	}
	a, err := launchable(ctx, s.db, job.SpaceID, p.App)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrOffline) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.liveDef == nil {
		return nil
	}
	sch, ok := a.liveDef.Schedules[p.Name]
	if !ok {
		return nil
	}
	planned, err := scheduledContext(sch, job.RunAt)
	if err != nil {
		return schedule.Permanent(err)
	}
	_, err = s.RunAction(ctx, Actor{UserID: job.UserID}, job.SpaceID, p.App, ActionRun{
		Action: sch.Action, Context: planned, ExpectedRelease: a.Live.Number, Operation: scheduledOperation(job),
	})
	switch {
	case err == nil:
		return s.settleSchedule(ctx, job, p, sch, "")
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrNotFound), errors.Is(err, ErrReadOnly):
		return s.failSchedule(ctx, job, p, scheduleErrAccess)
	case errors.Is(err, ErrFull):
		return s.settleSchedule(ctx, job, p, sch, scheduleErrFull)
	case errors.Is(err, ErrInvalid):
		return s.settleSchedule(ctx, job, p, sch, scheduleErrInvalid)
	}
	return err
}

func (s *Service) failSchedule(ctx context.Context, job schedule.Job, p schedulePayload, code string) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := s.noteSchedule(ctx, tx, p.App, p.Name, code, false); err != nil {
			return err
		}
		return notifyScheduleFailed(ctx, tx, job, p)
	})
	if err != nil {
		return err
	}
	return schedule.ErrAccessLost
}

func (s *Service) settleSchedule(ctx context.Context, job schedule.Job, p schedulePayload, sch Schedule, code string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		holds, err := s.sched.Holds(ctx, tx, job)
		if err != nil || !holds {
			return err
		}
		if err := s.noteSchedule(ctx, tx, p.App, p.Name, code, code == ""); err != nil {
			return err
		}
		if code != "" {
			if err := notifyScheduleFailed(ctx, tx, job, p); err != nil {
				return err
			}
		}
		after := s.sched.Now()
		if job.RunAt.After(after) {
			after = job.RunAt
		}
		next, err := sch.Calendar.Next(after)
		if err != nil {
			return err
		}
		job.RunAt = next
		_, err = s.sched.Reschedule(ctx, tx, job)
		return err
	})
}

func notifyScheduleFailed(ctx context.Context, tx *sql.Tx, job schedule.Job, p schedulePayload) error {
	return notifications.Add(ctx, tx, notifications.Notice{
		UserID: job.UserID, WorkspaceID: job.WorkspaceID, Kind: notifications.KindAppScheduleFailed,
		Ref:       notifications.Ref{Space: job.SpaceID, App: p.App},
		DedupeKey: "schedule:" + job.Occurrence(),
	})
}
