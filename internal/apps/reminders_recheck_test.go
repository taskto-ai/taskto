package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/schedule"
)

func newRemindFXWith(t *testing.T, before func(*remindFX, *schedule.Job), reminders ...Reminder) *remindFX {
	t.Helper()
	w := newWorld(t)
	clock := &remindClock{t: utc(2030, time.January, 2, 9, 0)}
	sch := schedule.New(w.e.db, w.e.s.access, schedule.Options{Clock: clock.now})
	f := &remindFX{world: w, clock: clock, sch: sch}
	err := sch.Register(KindRecordReminder, func(ctx context.Context, job schedule.Job) error {
		if before != nil {
			before(f, &job)
		}
		err := w.e.s.remindRecord(ctx, job)
		if f.after != nil {
			return f.after(err)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	w.e.s.WithScheduler(sch)
	w.e.s.access.OnArchiveChange(w.e.s.SyncSpaceReminders)
	w.e.s.access.OnMemberAdded(w.e.s.SyncSpaceMemberReminders)
	w.e.accounts.OnMemberAdded(w.e.s.SyncWorkspaceMemberReminders)
	w.e.accounts.OnTimeZoneChanged(w.e.s.SyncUserReminders)
	w.e.accounts.OnMemberEnabled(w.e.s.SyncUserReminders)
	w.e.s.access.OnAppGrantsChanged(w.e.s.SyncAppGrantReminders)
	w.e.addMember(w.owner, w.shared, w.mallory, access.RoleEditor)
	rec := w.alice.do(http.MethodPost, appsPath(w.shared, ""), map[string]any{"name": "Dues", "definition": remindDefinition(reminders...)})
	expect(t, rec, http.StatusCreated)
	f.app = w.publish(w.alice, w.shared, decode[App](t, rec))
	return f
}

func (f *remindFX) sql(query string, args ...any) {
	f.e.t.Helper()
	if _, err := f.e.db.Exec(query, args...); err != nil {
		f.e.t.Fatal(err)
	}
}

func (f *remindFX) spacePath() string {
	return fmt.Sprintf("/api/workspaces/%d/spaces/%d", f.ws, f.shared.ID)
}

func TestTheHandlerChecksEverythingAgainWhenTheJobFires(t *testing.T) {
	cases := map[string]func(f *remindFX, id int64, job *schedule.Job){
		"the record was deleted": func(f *remindFX, id int64, _ *schedule.Job) {
			f.sql(`UPDATE app_rows SET deleted_at = ? WHERE id = ?`, now(), id)
		},
		"someone else is now the creator": func(f *remindFX, id int64, _ *schedule.Job) {
			f.sql(`UPDATE app_rows SET created_by = ? WHERE id = ?`, f.mallory.id, id)
		},
		"the condition no longer holds": func(f *remindFX, id int64, _ *schedule.Job) {
			f.sql(`UPDATE app_rows SET data = json_set(data, '$.status', 'done') WHERE id = ?`, id)
		},
		"the date was taken away": func(f *remindFX, id int64, _ *schedule.Job) {
			f.sql(`UPDATE app_rows SET data = json_remove(data, '$.due') WHERE id = ?`, id)
		},
		"the live release has no reminder of that name": func(f *remindFX, id int64, job *schedule.Job) {
			job.Payload = json.RawMessage(fmt.Sprintf(`{"app":%d,"row":%d,"name":"gone"}`, f.app.ID, id))
		},
		"the space was archived": func(f *remindFX, _ int64, _ *schedule.Job) {
			f.sql(`UPDATE spaces SET archived_at = ? WHERE id = ?`, now(), f.shared.ID)
		},
		"the claim on the job was taken over": func(f *remindFX, _ int64, job *schedule.Job) {
			f.sql(`UPDATE scheduled_jobs SET lease = lease + 1 WHERE id = ?`, job.ID)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			var id int64
			f := newRemindFXWith(t, func(f *remindFX, job *schedule.Job) { change(f, id, job) }, dueReminder())
			id = f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
			f.run(utc(2030, time.January, 5, 9, 0))
			if n := len(f.notices(f.alice)); n != 0 {
				t.Fatalf("%d notices after %s", n, name)
			}
		})
	}
}

func TestAHandlerThatFindsTheOwnerWithoutAccessEndsTheJobAndOneThatFindsThemDisabledHoldsIt(t *testing.T) {
	ended := map[string]func(f *remindFX, job *schedule.Job){
		"the job names a space the owner can't open": func(f *remindFX, job *schedule.Job) {
			closed := f.e.createShared(f.owner, f.ws, "Closed")
			f.sql(`UPDATE scheduled_jobs SET space_id = ? WHERE id = ?`, closed.ID, job.ID)
		},
		"the job names another workspace than its space's": func(f *remindFX, job *schedule.Job) {
			job.WorkspaceID = f.ws + 100
		},
		"the owner holds no role in the app": func(f *remindFX, _ *schedule.Job) {
			f.sql(`DELETE FROM app_roles WHERE app_id = ?`, f.app.ID)
			f.sql(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, 'user')`, f.app.ID, f.mallory.id)
		},
		"the owner may open the app but read none of its records": func(f *remindFX, _ *schedule.Job) {
			f.sql(`DELETE FROM app_roles WHERE app_id = ?`, f.app.ID)
			f.sql(`INSERT INTO app_roles (app_id, user_id, role) VALUES (?, ?, 'builder'), (?, ?, 'user')`, f.app.ID, f.alice.id, f.app.ID, f.mallory.id)
		},
	}
	for name, change := range ended {
		t.Run(name, func(t *testing.T) {
			f := newRemindFXWith(t, func(f *remindFX, job *schedule.Job) { change(f, job) }, dueReminder())
			id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
			f.run(utc(2030, time.January, 5, 9, 0))
			if n := len(f.notices(f.alice)); n != 0 {
				t.Fatalf("%d notices", n)
			}
			if j, ok := f.job(f.alice, id, "due"); !ok || j.State != schedule.StateFailed {
				t.Fatalf("job = %+v (found %v), want it ended as failed", j, ok)
			}
		})
	}

	t.Run("the owner's account was disabled", func(t *testing.T) {
		f := newRemindFXWith(t, func(f *remindFX, _ *schedule.Job) {
			f.sql(`UPDATE users SET disabled_at = ?, disabled_by = ? WHERE id = ?`, now(), f.owner.id, f.alice.id)
		}, dueReminder())
		id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
		f.run(utc(2030, time.January, 5, 9, 0))
		if n := len(f.notices(f.alice)); n != 0 {
			t.Fatalf("%d notices", n)
		}
		if j, ok := f.job(f.alice, id, "due"); !ok || j.State != schedule.StatePending || j.Attempts != 0 {
			t.Fatalf("job = %+v (found %v), want it held and still pending with no try counted", j, ok)
		}
	})

	t.Run("a payload that does not read as a reminder", func(t *testing.T) {
		f := newRemindFXWith(t, nil, dueReminder())
		_, err := f.sch.Schedule(context.Background(), f.e.db, schedule.Job{
			Kind: KindRecordReminder, UserID: f.alice.id, WorkspaceID: f.ws, SpaceID: f.shared.ID,
			DedupeKey: "bad", Payload: json.RawMessage(`{"app":"x"}`), RunAt: utc(2030, time.January, 5, 9, 0),
		})
		if err != nil {
			t.Fatal(err)
		}
		f.run(utc(2030, time.January, 5, 9, 0))
		j, err := f.sch.Find(context.Background(), f.e.db, f.alice.id, KindRecordReminder, "bad")
		if err != nil {
			t.Fatal(err)
		}
		if j.State != schedule.StateFailed {
			t.Fatalf("job state = %q after one try, want failed: a payload that is wrong now is wrong on every retry", j.State)
		}
	})
}

func TestARecordThatChangesAfterItsReminderWasToldRemindsAgainAtItsNewDate(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	f.run(utc(2030, time.January, 5, 9, 0))
	if n := len(f.notices(f.alice)); n != 1 {
		t.Fatalf("%d notices, want 1", n)
	}
	f.edit(f.alice, id, map[string]any{"due": "2030-01-10"})
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 10, 9, 0))
	f.run(utc(2030, time.January, 10, 9, 0))
	if n := len(f.notices(f.alice)); n != 2 {
		t.Fatalf("%d notices, want one for each date", n)
	}
}

func TestAnArchivedSpaceRemindsNoOneUntilItIsRestored(t *testing.T) {
	f := newRemindFX(t, dueReminder())
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 9, 0))

	expect(t, f.owner.do(http.MethodPatch, f.spacePath(), map[string]any{"archived": true}), http.StatusOK)
	f.wantNoJob(f.alice, id, "due")
	expect(t, f.owner.do(http.MethodPatch, f.spacePath(), map[string]any{"archived": false}), http.StatusOK)
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 9, 0))
}

func TestACreatorWhoMayNoLongerReadRecordsKeepsNoJobWhenSomeoneElseChangesTheirRecord(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
	f.mustSetRoles(f.alice, f.shared, f.app,
		grant(f.alice.id, access.AppBuilder, access.AppPublisher, access.AppDataManager),
		grant(f.mallory.id, access.AppDataManager))
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 9, 0))

	f.mustSetRoles(f.alice, f.shared, f.app, grant(f.alice.id, access.AppBuilder), grant(f.mallory.id, access.AppDataManager))
	f.edit(f.mallory, id, map[string]any{"due": "2030-01-08"})
	f.wantNoJob(f.alice, id, "due")
}

func TestADateTheSchedulerCannotHoldCancelsTheJobThatWasThere(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 9, 0))
	f.edit(f.alice, id, map[string]any{"due": "9999-12-31"})
	f.wantNoJob(f.alice, id, "due")
}

func TestAUserAtTheSchedulersLimitCanStillSaveARecord(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
	for i := 0; i < schedule.MaxPerUser; i++ {
		_, err := f.sch.Schedule(context.Background(), f.e.db, schedule.Job{
			Kind: KindRecordReminder, UserID: f.alice.id, WorkspaceID: f.ws, SpaceID: f.shared.ID,
			DedupeKey: fmt.Sprintf("filler-%d", i), Payload: json.RawMessage(`{"app":0,"row":0,"name":"x"}`),
			RunAt: utc(2031, time.January, 5, 9, 0),
		})
		if err != nil {
			t.Fatalf("filler %d: %v", i, err)
		}
	}
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	f.wantNoJob(f.alice, id, "due")
}

func TestTheTextOfAReminderIsNotShownForAnArchivedAppOrSpaceOrAnotherWorkspace(t *testing.T) {
	f := newRemindFX(t, dueReminder())
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	if _, ok := f.text(f.alice, id, "due"); !ok {
		t.Fatal("the text of a reminder is not shown to its creator")
	}
	if _, ok, err := f.e.s.ReminderText(context.Background(), f.e.s.access, f.alice.id, f.ws+1, notifications.Ref{Space: f.shared.ID, App: f.app.ID, Record: id, Reminder: "due"}); err != nil || ok {
		t.Fatalf("text for another workspace: shown %v, err %v", ok, err)
	}

	f.sql(`UPDATE apps SET archived_at = ? WHERE id = ?`, now(), f.app.ID)
	if _, ok := f.text(f.alice, id, "due"); ok {
		t.Fatal("the text of a reminder of an archived app is shown")
	}
	f.sql(`UPDATE apps SET archived_at = NULL WHERE id = ?`, f.app.ID)
	f.sql(`UPDATE spaces SET archived_at = ? WHERE id = ?`, now(), f.shared.ID)
	if _, ok := f.text(f.alice, id, "due"); ok {
		t.Fatal("the text of a reminder of an archived space is shown")
	}
}

func TestAReminderTitleOfTwoHundredCharactersIsAcceptedAndOneMoreIsNot(t *testing.T) {
	title := func(n int) Reminder { return Reminder{Name: "due", Date: "due", Title: strings.Repeat("x", n)} }
	if _, err := remindDefinition(title(200)).Check(); err != nil {
		t.Fatalf("200 characters: %v", err)
	}
	if _, err := remindDefinition(title(201)).Check(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("201 characters: %v, want ErrInvalid", err)
	}
}
