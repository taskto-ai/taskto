package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/schedule"
)

const lunch = "weekday_lunch"

var (
	friday = time.Date(2026, 12, 4, 10, 0, 0, 0, time.UTC)
	monday = time.Date(2026, 12, 7, 9, 0, 0, 0, time.UTC)
)

type sched struct {
	*world
	clock time.Time
	jobs  *schedule.Service
}

func newSched(t *testing.T) *sched {
	t.Helper()
	s := &sched{world: newWorld(t), clock: friday}
	s.jobs = schedule.New(s.e.db, s.e.s.access, schedule.Options{Clock: func() time.Time { return s.clock }})
	if err := s.e.s.EnableSchedules(s.jobs); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *sched) definition() Definition {
	d := example(s.e.t)
	d.Collection.Fields = append(slices.Clone(d.Collection.Fields), Field{Name: "day", Type: TypeDate})
	d.Schedules = map[string]Schedule{lunch: {
		Action:      "add_expense",
		Calendar:    schedule.Calendar{Days: []string{"mon", "tue", "wed", "thu", "fri"}, At: "09:00", Zone: "Europe/Lisbon"},
		Values:      map[string]any{"description": "Lunch", "amount": float64(12)},
		TodayFields: []string{"day"},
	}}
	return d
}

func (s *sched) create(u *user, sp access.Space, def Definition) App {
	s.e.t.Helper()
	rec := u.do(http.MethodPost, appsPath(sp, ""), map[string]any{"name": "Scheduled", "definition": def})
	expect(s.e.t, rec, http.StatusCreated)
	return s.publish(u, sp, decode[App](s.e.t, rec))
}

func (s *sched) current(u *user, sp access.Space, a App) App { return s.view(u, sp, a) }

func (s *sched) job(a App, runner int64) (schedule.Job, error) {
	return s.jobs.Find(context.Background(), s.e.db, runner, KindScheduledAction, scheduleKey(a.ID, lunch))
}

func (s *sched) mustJob(a App, runner int64) schedule.Job {
	s.e.t.Helper()
	j, err := s.job(a, runner)
	if err != nil {
		s.e.t.Fatalf("no job of user %d for the app: %v", runner, err)
	}
	return j
}

func (s *sched) noJob(a App, runner int64) {
	s.e.t.Helper()
	if j, err := s.job(a, runner); !errors.Is(err, schedule.ErrNotFound) {
		s.e.t.Fatalf("a job of user %d is left: %+v, %v", runner, j, err)
	}
}

func (s *sched) records(a App) []map[string]any {
	s.e.t.Helper()
	rows, err := s.e.db.Query(`SELECT data FROM app_rows WHERE app_id = ? AND deleted_at IS NULL ORDER BY id`, a.ID)
	if err != nil {
		s.e.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			s.e.t.Fatal(err)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			s.e.t.Fatal(err)
		}
		out = append(out, data)
	}
	return out
}

func (s *sched) lunches(a App) int {
	n := 0
	for _, r := range s.records(a) {
		if r["description"] == "Lunch" {
			n++
		}
	}
	return n
}

func (s *sched) exec(query string, args ...any) {
	s.e.t.Helper()
	if _, err := s.e.db.Exec(query, args...); err != nil {
		s.e.t.Fatal(err)
	}
}

func (s *sched) one(dest any, query string, args ...any) {
	s.e.t.Helper()
	if err := s.e.db.QueryRow(query, args...).Scan(dest); err != nil {
		s.e.t.Fatal(err)
	}
}

func (s *sched) at(t time.Time) { s.clock = t }

func (s *sched) runDue() { s.jobs.RunDue(context.Background()) }

func (s *sched) lastError(a App) string {
	s.e.t.Helper()
	var code *string
	s.one(&code, `SELECT last_error FROM app_schedules WHERE app_id = ? AND name = ?`, a.ID, lunch)
	if code == nil {
		return ""
	}
	return *code
}

func (s *sched) notices(user int64) int {
	s.e.t.Helper()
	var n int
	s.one(&n, `SELECT count(*) FROM notifications WHERE user_id = ? AND kind = ?`, user, notifications.KindAppScheduleFailed)
	return n
}

func TestDefinitionChecksSchedules(t *testing.T) {
	s := newSched(t)
	cases := map[string]func(d *Definition){
		"an update action": func(d *Definition) {
			sc := d.Schedules[lunch]
			sc.Action = "remove"
			d.Actions["remove"] = Action{Kind: ActionDelete}
			d.Schedules[lunch] = sc
		},
		"an unknown action": func(d *Definition) {
			sc := d.Schedules[lunch]
			sc.Action = "nothing"
			d.Schedules[lunch] = sc
		},
		"an unknown field": func(d *Definition) { d.Schedules[lunch].Values["nowhere"] = "x" },
		"a value of the wrong type": func(d *Definition) {
			d.Schedules[lunch].Values["amount"] = "plenty"
		},
		"a today field that is not a date": func(d *Definition) {
			sc := d.Schedules[lunch]
			delete(sc.Values, "description")
			sc.TodayFields = []string{"description"}
			d.Schedules[lunch] = sc
		},
		"a field set twice": func(d *Definition) { d.Schedules[lunch].Values["day"] = "2026-01-01" },
		"a record that does not fit": func(d *Definition) {
			delete(d.Schedules[lunch].Values, "description")
		},
		"a calendar with no zone": func(d *Definition) {
			sc := d.Schedules[lunch]
			sc.Calendar.Zone = ""
			d.Schedules[lunch] = sc
		},
		"a name that is not a name": func(d *Definition) { d.Schedules["Not A Name"] = d.Schedules[lunch] },
		"six schedules": func(d *Definition) {
			for i := 0; i < 5; i++ {
				d.Schedules[fmt.Sprintf("more_%d", i)] = d.Schedules[lunch]
			}
		},
		"definition version 3": func(d *Definition) { d.DefinitionVersion = 3 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := s.definition()
			mutate(&d)
			if _, err := d.Check(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Check = %v, want it refused", err)
			}
		})
	}
	for name, field := range map[string]Field{
		"an attachment field": {Name: "photo", Type: TypeAttachment},
		"a location field":    {Name: "photo", Type: TypeLocation},
	} {
		t.Run(name, func(t *testing.T) {
			d := s.definition()
			d.Collection.Fields = append(d.Collection.Fields, field)
			d.Schedules[lunch].Values["photo"] = map[string]any{"lat": 38.7, "lon": -9.1, "source": "manual", "captured_at": "2026-01-01T00:00:00Z"}
			if _, err := d.Check(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Check = %v, want it refused", err)
			}
		})
	}
	t.Run("an action that only opens a screen", func(t *testing.T) {
		d := planner(t)
		d.Schedules = map[string]Schedule{lunch: {Action: "new_task",
			Calendar: schedule.Calendar{At: "09:00", Zone: "Europe/Lisbon"}}}
		if _, err := d.Check(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Check = %v, want it refused", err)
		}
	})
	t.Run("a valid one", func(t *testing.T) {
		d, err := s.definition().Check()
		if err != nil {
			t.Fatal(err)
		}
		if d.DefinitionVersion != DefinitionVersion || !slices.Contains(d.Requires, SchedulesCapability) {
			t.Fatalf("version %d, requires %v", d.DefinitionVersion, d.Requires)
		}
	})
}

func TestPublishPlansTheJobAtTheNextOccurrenceAsThePublisher(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	job := s.mustJob(a, s.alice.id)
	if !job.RunAt.Equal(monday) || job.SpaceID != s.shared.ID || job.WorkspaceID != s.ws {
		t.Fatalf("job = %+v, want %s in the app's space", job, monday)
	}
	if strings.Contains(string(job.Payload), "Lunch") || strings.Contains(string(job.Payload), "12") {
		t.Fatalf("the payload holds record values: %s", job.Payload)
	}
	var runner int64
	s.one(&runner, `SELECT runner_id FROM app_schedules WHERE app_id = ? AND name = ?`, a.ID, lunch)
	if runner != s.alice.id {
		t.Fatalf("runner = %d, want %d", runner, s.alice.id)
	}
}

func TestPublishingWithoutTheScheduleCancelsItsJob(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	plain := example(t)
	rec := s.alice.do(http.MethodPatch, appPath(s.shared, a.ID, ""), map[string]any{"version": s.current(s.alice, s.shared, a).Version, "definition": plain})
	expect(t, rec, http.StatusOK)
	s.publish(s.alice, s.shared, decode[App](t, rec))
	s.noJob(a, s.alice.id)
	var n int
	s.one(&n, `SELECT count(*) FROM app_schedules WHERE app_id = ?`, a.ID)
	if n != 0 {
		t.Fatalf("%d schedule rows are left", n)
	}
}

func TestAPublishThatKeepsTheScheduleLeavesItsJobAlone(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	before := s.mustJob(a, s.alice.id)
	d := s.definition()
	d.Collection.Fields = append(d.Collection.Fields, Field{Name: "note", Type: TypeString})
	rec := s.alice.do(http.MethodPatch, appPath(s.shared, a.ID, ""), map[string]any{"version": s.current(s.alice, s.shared, a).Version, "definition": d})
	expect(t, rec, http.StatusOK)
	s.publish(s.alice, s.shared, decode[App](t, rec))
	after := s.mustJob(a, s.alice.id)
	if after.ID != before.ID || after.Revision != before.Revision {
		t.Fatalf("job was replaced: %+v then %+v", before, after)
	}
}

func TestRollbackPlansTheJobAsThePersonRollingBack(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	rec := s.alice.do(http.MethodPatch, appPath(s.shared, a.ID, ""), map[string]any{"version": s.current(s.alice, s.shared, a).Version, "definition": example(t)})
	expect(t, rec, http.StatusOK)
	a = s.publish(s.alice, s.shared, decode[App](t, rec))
	s.noJob(a, s.alice.id)
	expect(t, s.owner.do(http.MethodPost, appPath(s.shared, a.ID, "/rollback"), map[string]any{"version": s.current(s.owner, s.shared, a).Version, "release": 1}), http.StatusOK)
	s.noJob(a, s.alice.id)
	if job := s.mustJob(a, s.owner.id); job.UserID != s.owner.id || !job.RunAt.Equal(monday) {
		t.Fatalf("job = %+v", job)
	}
}

func TestAPublishByAnotherPersonMovesTheScheduleToThem(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	d := s.definition()
	d.Collection.Fields = append(d.Collection.Fields, Field{Name: "note", Type: TypeString})
	rec := s.owner.do(http.MethodPatch, appPath(s.shared, a.ID, ""), map[string]any{"version": s.current(s.owner, s.shared, a).Version, "definition": d})
	expect(t, rec, http.StatusOK)
	s.publish(s.owner, s.shared, decode[App](t, rec))
	s.noJob(a, s.alice.id)
	s.mustJob(a, s.owner.id)
	s.at(monday.Add(time.Minute))
	s.runDue()
	var by int64
	s.one(&by, `SELECT created_by FROM app_rows WHERE app_id = ? ORDER BY id DESC LIMIT 1`, a.ID)
	if by != s.owner.id || s.lunches(a) != 1 {
		t.Fatalf("created by %d with %d lunches, want one by the new publisher %d", by, s.lunches(a), s.owner.id)
	}
}

func TestArchivingCancelsTheJobAndRestoringPlansItAgainForTheSameRunner(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	patch := func(archived bool) {
		expect(t, s.alice.do(http.MethodPatch, appPath(s.shared, a.ID, ""), map[string]any{"version": s.current(s.alice, s.shared, a).Version, "archived": archived}), http.StatusOK)
	}
	patch(true)
	s.noJob(a, s.alice.id)
	var n int
	s.one(&n, `SELECT count(*) FROM app_schedules WHERE app_id = ?`, a.ID)
	if n != 1 {
		t.Fatalf("archiving dropped the schedule: %d rows", n)
	}
	patch(false)
	if job := s.mustJob(a, s.alice.id); !job.RunAt.Equal(monday) {
		t.Fatalf("job = %+v", job)
	}
}

func TestMovingTheAppMovesTheJobOnlyWhereTheRunnerCanRead(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	stale := s.mustJob(a, s.alice.id)
	move := func(u *user, to access.Space) {
		expect(t, u.do(http.MethodPost, appPath(s.shared, a.ID, "/move"), map[string]any{"version": s.current(u, s.shared, a).Version, "space_id": to.ID}), http.StatusOK)
	}
	move(s.alice, s.alicePrivate)
	if job := s.mustJob(a, s.alice.id); job.SpaceID != s.alicePrivate.ID {
		t.Fatalf("job = %+v, want the space it moved to", job)
	}
	s.exec(`UPDATE apps SET space_id = ? WHERE id = ?`, s.shared.ID, a.ID)
	ops := s.e.createShared(s.owner, s.ws, "Ops")
	expect(t, s.owner.do(http.MethodPost, appPath(s.shared, a.ID, "/move"), map[string]any{"version": s.current(s.owner, s.shared, a).Version, "space_id": ops.ID}), http.StatusOK)
	s.noJob(a, s.alice.id)
	before := len(s.records(a))
	if err := s.e.s.runScheduled(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if len(s.records(a)) != before {
		t.Fatal("a job of the old space inserted into the moved app")
	}
}

func TestADueJobInsertsOneRecordInTheCalendarsZoneAndPlansTheNext(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	s.at(monday.Add(time.Minute))
	s.runDue()
	got := s.records(a)
	last := got[len(got)-1]
	if s.lunches(a) != 1 || last["day"] != "2026-12-07" || last["amount"] != float64(12) {
		t.Fatalf("records = %v", got)
	}
	var by int64
	s.one(&by, `SELECT created_by FROM app_rows WHERE app_id = ? ORDER BY id DESC LIMIT 1`, a.ID)
	if by != s.alice.id {
		t.Fatalf("the record was made by %d, want the runner %d", by, s.alice.id)
	}
	if job := s.mustJob(a, s.alice.id); !job.RunAt.Equal(monday.Add(24 * time.Hour)) {
		t.Fatalf("next = %s", job.RunAt)
	}
	s.at(monday.Add(24*time.Hour + time.Minute))
	s.runDue()
	if s.lunches(a) != 2 {
		t.Fatalf("the next day's run inserted %d records in all, want 2", s.lunches(a))
	}
}

func TestTheDateIsTheCalendarsDayNotUTCs(t *testing.T) {
	s := newSched(t)
	d := s.definition()
	d.Schedules[lunch] = Schedule{
		Action: "add_expense", Values: d.Schedules[lunch].Values, TodayFields: []string{"day"},
		Calendar: schedule.Calendar{Days: []string{"mon"}, At: "08:00", Zone: "Pacific/Auckland"},
	}
	a := s.create(s.alice, s.shared, d)
	job := s.mustJob(a, s.alice.id)
	s.at(job.RunAt.Add(time.Minute))
	s.runDue()
	if got := s.records(a); got[len(got)-1]["day"] != "2026-12-07" {
		t.Fatalf("day = %v, a run at %s UTC is Monday in Auckland", got[len(got)-1]["day"], job.RunAt)
	}
}

func TestARunThatIsTriedAgainInsertsOnce(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	job := s.mustJob(a, s.alice.id)
	s.at(monday.Add(time.Minute))
	for i := 0; i < 2; i++ {
		if err := s.e.s.runScheduled(context.Background(), job); err != nil {
			t.Fatal(err)
		}
	}
	s.runDue()
	if s.lunches(a) != 1 {
		t.Fatalf("%d records after three tries of one occurrence, want 1", s.lunches(a))
	}
}

func TestTheNextOccurrenceIsPlannedOnlyByTheTryThatHoldsTheLease(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	job := s.mustJob(a, s.alice.id)
	s.at(monday.Add(time.Minute))
	if err := s.e.s.runScheduled(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if after := s.mustJob(a, s.alice.id); !after.RunAt.Equal(job.RunAt) || after.Revision != job.Revision {
		t.Fatalf("a try with no lease moved the job: %+v", after)
	}
}

func TestARunAfterTheRunnerLostTheSpaceInsertsNothing(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	job := s.mustJob(a, s.alice.id)
	s.exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, s.shared.ID, s.alice.id)
	s.noJob(a, s.alice.id)
	s.at(monday.Add(time.Minute))
	s.runDue()
	if err := s.e.s.runScheduled(context.Background(), job); !errors.Is(err, schedule.ErrAccessLost) {
		t.Fatalf("handler = %v, want access lost", err)
	}
	if s.lunches(a) != 0 {
		t.Fatal("a person who left the space still inserted a record")
	}
}

func TestARunAfterTheRunnerLeftTheWorkspaceInsertsNothingAndTellsNoOne(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	job := s.mustJob(a, s.alice.id)
	s.exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, s.ws, s.alice.id)
	s.noJob(a, s.alice.id)
	s.at(monday.Add(time.Minute))
	s.runDue()
	if err := s.e.s.runScheduled(context.Background(), job); !errors.Is(err, schedule.ErrAccessLost) {
		t.Fatalf("handler = %v, want access lost", err)
	}
	if s.lunches(a) != 0 || s.notices(s.alice.id) != 0 {
		t.Fatalf("records %d, notices %d", s.lunches(a), s.notices(s.alice.id))
	}
}

func TestADisabledRunnerIsHeldAndInsertsNothing(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	s.exec(`UPDATE users SET disabled_at = ? WHERE id = ?`, friday.Format(time.RFC3339), s.alice.id)
	s.at(monday.Add(time.Minute))
	s.runDue()
	if s.lunches(a) != 0 {
		t.Fatal("a disabled person's schedule ran")
	}
	if job := s.mustJob(a, s.alice.id); job.State != schedule.StatePending {
		t.Fatalf("job = %+v, want it held for when they come back", job)
	}
}

func TestARunnerWhoseRoleWasLoweredInsertsNothingAndEndsTheJob(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	s.exec(`UPDATE space_members SET role = 'viewer' WHERE space_id = ? AND user_id = ?`, s.shared.ID, s.alice.id)
	s.at(monday.Add(time.Minute))
	s.runDue()
	if s.lunches(a) != 0 {
		t.Fatal("a viewer's schedule inserted a record")
	}
	if job := s.mustJob(a, s.alice.id); job.State != schedule.StateFailed {
		t.Fatalf("job = %+v, want it ended", job)
	}
	if s.lastError(a) != scheduleErrAccess || s.notices(s.alice.id) != 1 {
		t.Fatalf("last_error %q, notices %d", s.lastError(a), s.notices(s.alice.id))
	}
}

func TestARunOfAnUnpublishedOrArchivedAppInsertsNothing(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	job := s.mustJob(a, s.alice.id)
	expect(t, s.alice.do(http.MethodPatch, appPath(s.shared, a.ID, ""), map[string]any{"version": s.current(s.alice, s.shared, a).Version, "archived": true}), http.StatusOK)
	if err := s.e.s.runScheduled(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	s.exec(`UPDATE apps SET archived_at = NULL, live_release = NULL WHERE id = ?`, a.ID)
	if err := s.e.s.runScheduled(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if s.lunches(a) != 0 {
		t.Fatal("an app that wasn't live took a record")
	}
}

func TestAnotherMemberNeverBecomesTheActor(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	job := s.mustJob(a, s.alice.id)
	forged := job
	forged.UserID = s.owner.id
	s.at(monday.Add(time.Minute))
	if err := s.e.s.runScheduled(context.Background(), forged); err != nil {
		t.Fatal(err)
	}
	if s.lunches(a) != 0 {
		t.Fatal("a job of someone who didn't publish the release inserted a record")
	}
}

func TestAScheduleWithoutARunnerIsNotPlanned(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	s.exec(`UPDATE app_schedules SET runner_id = NULL WHERE app_id = ?`, a.ID)
	s.exec(`DELETE FROM scheduled_jobs`)
	patch := func(archived bool) {
		expect(t, s.alice.do(http.MethodPatch, appPath(s.shared, a.ID, ""), map[string]any{"version": s.current(s.alice, s.shared, a).Version, "archived": archived}), http.StatusOK)
	}
	patch(true)
	patch(false)
	var n int
	s.one(&n, `SELECT count(*) FROM scheduled_jobs WHERE kind = ?`, string(KindScheduledAction))
	if n != 0 {
		t.Fatalf("%d jobs planned with no runner", n)
	}
}

func TestARecordTheCollectionCannotTakeIsNotedAndTheNextRunStillPlanned(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	s.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO app_rows (app_id, data, created_by) SELECT ?, '{"description":"x","amount":1}', ? FROM n`, MaxRows, a.ID, s.alice.id)
	s.at(monday.Add(time.Minute))
	s.runDue()
	if s.lunches(a) != 0 || s.lastError(a) != scheduleErrFull || s.notices(s.alice.id) != 1 {
		t.Fatalf("lunches %d, last_error %q, notices %d", s.lunches(a), s.lastError(a), s.notices(s.alice.id))
	}
	if job := s.mustJob(a, s.alice.id); !job.RunAt.Equal(monday.Add(24 * time.Hour)) {
		t.Fatalf("next = %s", job.RunAt)
	}
}

func TestRemovingAnAppRemovesItsJobs(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	s.mustJob(a, s.alice.id)
	s.exec(`DELETE FROM apps WHERE id = ?`, a.ID)
	s.noJob(a, s.alice.id)
}

func TestAJobWhoseScheduleIsGoneInsertsNothing(t *testing.T) {
	s := newSched(t)
	a := s.create(s.alice, s.shared, s.definition())
	stale := s.mustJob(a, s.alice.id)
	s.exec(`DELETE FROM app_schedules WHERE app_id = ?`, a.ID)
	s.at(monday.Add(time.Minute))
	if err := s.e.s.runScheduled(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if s.lunches(a) != 0 {
		t.Fatal("a job outlived its schedule and inserted a record")
	}
}
