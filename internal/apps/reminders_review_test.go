package apps

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/schedule"
)

func (f *remindFX) moveTo(u *user, to access.Space) {
	f.e.t.Helper()
	cur := f.view(u, f.shared, f.app)
	expect(f.e.t, u.do(http.MethodPost, appPath(f.shared, f.app.ID, "/move"), map[string]any{"version": cur.Version, "space_id": to.ID}), http.StatusOK)
}

func (f *remindFX) fill(user *user, n int) {
	f.e.t.Helper()
	for i := range n {
		_, err := f.sch.Schedule(context.Background(), f.e.db, schedule.Job{
			Kind: KindRecordReminder, UserID: user.id, WorkspaceID: f.ws, SpaceID: f.shared.ID,
			DedupeKey: fmt.Sprintf("filler-%d", i), Payload: []byte(`{"app":0,"row":0,"name":"x"}`),
			RunAt: utc(2031, time.January, 5, 9, 0),
		})
		if err != nil {
			f.e.t.Fatalf("filler %d: %v", i, err)
		}
	}
}

func (f *remindFX) wantNoticesIn(u *user, space access.Space, n int) {
	f.e.t.Helper()
	refs := f.notices(u)
	if len(refs) != n {
		f.e.t.Fatalf("%d notices %v, want %d", len(refs), refs, n)
	}
	for _, ref := range refs {
		if !strings.Contains(ref, fmt.Sprintf(`"space":%d,`, space.ID)) {
			f.e.t.Fatalf("notice %s is not of space %d", ref, space.ID)
		}
	}
}

func TestAClaimedReminderIsToldOnceWhenTheAppMovesBeforeTheHandlerStarts(t *testing.T) {
	var other access.Space
	moved := false
	f := newRemindFXWith(t, func(f *remindFX, _ *schedule.Job) {
		if !moved {
			moved = true
			f.moveTo(f.alice, other)
		}
	}, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
	other = f.e.createShared(f.owner, f.ws, "Elsewhere")
	f.e.addMember(f.owner, other, f.alice, access.RoleEditor)
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})

	f.run(utc(2030, time.January, 5, 9, 0))
	if !moved {
		t.Fatal("the app never moved")
	}
	f.wantNoticesIn(f.alice, other, 1)
	if j, ok := f.job(f.alice, id, "due"); !ok || j.SpaceID != other.ID || j.State != schedule.StateDone {
		t.Fatalf("job = %+v (found %v), want it done in the new space", j, ok)
	}
}

func TestAReminderAwaitingRetryIsNotToldTwiceWhenTheAppMoves(t *testing.T) {
	f := newRemindFXWith(t, nil, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
	other := f.e.createShared(f.owner, f.ws, "Elsewhere")
	f.e.addMember(f.owner, other, f.alice, access.RoleEditor)
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	failed := false
	f.after = func(err error) error {
		if err == nil && !failed {
			failed = true
			return errors.New("the handler ran past its time limit")
		}
		return err
	}

	f.run(utc(2030, time.January, 5, 9, 0))
	if j, ok := f.job(f.alice, id, "due"); !ok || j.State != schedule.StatePending {
		t.Fatalf("job = %+v (found %v), want it pending a retry", j, ok)
	}
	before, _ := f.job(f.alice, id, "due")
	f.moveTo(f.alice, other)
	after, _ := f.job(f.alice, id, "due")
	if after.SpaceID != other.ID || after.Revision != before.Revision || after.State != schedule.StatePending {
		t.Fatalf("job before %+v, after %+v: want the same occurrence in the new space", before, after)
	}

	f.run(utc(2030, time.January, 5, 9, 1))
	if n := len(f.notices(f.alice)); n != 1 {
		t.Fatalf("%d notices, want the one occurrence told once", n)
	}
}

func TestAReminderAtATimeThatExistsAfterAHalfHourClockChangeKeepsItsTime(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Time: "starts", Title: "{{title}}"})
	f.zone(f.alice, "Australia/Lord_Howe")
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-10-06", "starts": "02:45"})
	f.wantJob(f.alice, id, "due", utc(2030, time.October, 5, 15, 45))
}

func TestACreatorWhoRegainsTheAppBeforeTheNewDateIsRemindedThen(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
	full := []access.AppRole{access.AppBuilder, access.AppPublisher, access.AppDataManager}
	f.mustSetRoles(f.alice, f.shared, f.app, grant(f.alice.id, full...), grant(f.mallory.id, access.AppDataManager))
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	f.mustSetRoles(f.alice, f.shared, f.app, grant(f.alice.id, access.AppBuilder), grant(f.mallory.id, access.AppDataManager))
	f.edit(f.mallory, id, map[string]any{"due": "2030-01-08"})
	f.wantNoJob(f.alice, id, "due")

	f.mustSetRoles(f.alice, f.shared, f.app, grant(f.alice.id, full...), grant(f.mallory.id, access.AppDataManager))
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 8, 9, 0))
	f.run(utc(2030, time.January, 8, 9, 0))
	if n := len(f.notices(f.alice)); n != 1 {
		t.Fatalf("%d notices, want 1", n)
	}
}

func TestACreatorWhoIsEnabledAgainBeforeTheNewDateIsRemindedThen(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
	f.mustSetRoles(f.alice, f.shared, f.app,
		grant(f.alice.id, access.AppBuilder, access.AppPublisher, access.AppDataManager),
		grant(f.mallory.id, access.AppDataManager))
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	member := fmt.Sprintf("/api/workspaces/%d/members/%d/", f.ws, f.alice.id)
	expect(t, f.owner.do(http.MethodPost, member+"disable", nil), http.StatusNoContent)
	f.edit(f.mallory, id, map[string]any{"due": "2030-01-08"})
	f.wantNoJob(f.alice, id, "due")

	expect(t, f.owner.do(http.MethodPost, member+"enable", nil), http.StatusNoContent)
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 8, 9, 0))
	f.run(utc(2030, time.January, 8, 9, 0))
	if n := len(f.notices(f.alice)); n != 1 {
		t.Fatalf("%d notices, want 1", n)
	}
}

func TestARemovedReminderDoesNotHoldTheSlotItsReplacementNeeds(t *testing.T) {
	t.Run("a release renames it", func(t *testing.T) {
		f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
		id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
		f.fill(f.alice, schedule.MaxPerUser-1)

		f.republish(remindDefinition(Reminder{Name: "later", Date: "due", Title: "{{title}}"}))
		f.wantJob(f.alice, id, "later", utc(2030, time.January, 5, 9, 0))
		f.wantNoJob(f.alice, id, "due")
	})
	t.Run("a release drops the last one", func(t *testing.T) {
		f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
		id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
		f.republish(remindDefinition())
		f.wantNoJob(f.alice, id, "due")
	})
	t.Run("another record's job and another app's are left alone", func(t *testing.T) {
		f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"}, Reminder{Name: "second", Date: "due", Title: "{{title}}"})
		id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
		other := f.insert(f.mallory, map[string]any{"title": "tax", "due": "2030-01-06"})
		f.fill(f.alice, 3)
		f.republish(remindDefinition(Reminder{Name: "second", Date: "due", Title: "{{title}}"}))
		f.wantNoJob(f.alice, id, "due")
		f.wantJob(f.alice, id, "second", utc(2030, time.January, 5, 9, 0))
		f.wantJob(f.mallory, other, "second", utc(2030, time.January, 6, 9, 0))
		if n, err := f.sch.Find(context.Background(), f.e.db, f.alice.id, KindRecordReminder, "filler-0"); err != nil || n.State != schedule.StatePending {
			t.Fatalf("a job that is not of this app = %+v, %v", n, err)
		}
	})
}

func TestARetryAfterTheNoticeCommittedButBeforeTheJobSettledTellsOnce(t *testing.T) {
	runs := 0
	f := newRemindFXWith(t, func(*remindFX, *schedule.Job) { runs++ }, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
	f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	ctx, stop := context.WithCancel(context.Background())
	f.after = func(err error) error {
		stop()
		return err
	}
	f.clock.set(utc(2030, time.January, 5, 9, 0))
	f.sch.RunDue(ctx)
	if n := len(f.notices(f.alice)); n != 1 {
		t.Fatalf("%d notices after the first try, want 1", n)
	}

	f.after = nil
	f.run(utc(2030, time.January, 5, 9, 5))
	if runs != 2 {
		t.Fatalf("the handler ran %d times, want the lease's expiry to run it again", runs)
	}
	if n := len(f.notices(f.alice)); n != 1 {
		t.Fatalf("%d notices after the second try, want the same occurrence told once", n)
	}
}
