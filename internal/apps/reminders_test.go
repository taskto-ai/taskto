package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/schedule"
)

type remindClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *remindClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *remindClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func utc(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

type remindFX struct {
	*world
	clock *remindClock
	sch   *schedule.Service
	app   App
}

func dueReminder() Reminder {
	return Reminder{
		Name: "due", Date: "due", Time: "starts", Title: "Pay {{title}}",
		Where: &Rule{Field: "status", Op: OpNe, Value: "done"},
	}
}

func remindDefinition(reminders ...Reminder) Definition {
	return Definition{
		Components: []json.RawMessage{json.RawMessage(`{"id":"root","component":"Text","text":"Dues"}`)},
		Collection: Collection{Fields: []Field{
			{Name: "title", Type: TypeString, Required: true, MaxLength: 100},
			{Name: "due", Type: TypeDate},
			{Name: "starts", Type: TypeTime},
			{Name: "status", Type: TypeChoice, Options: []string{"open", "done"}},
		}},
		Actions: map[string]Action{
			"add":    {Kind: ActionInsert},
			"edit":   {Kind: ActionUpdate},
			"remove": {Kind: ActionDelete},
		},
		Reminders: reminders,
	}
}

func newRemindFX(t *testing.T, reminders ...Reminder) *remindFX {
	t.Helper()
	w := newWorld(t)
	clock := &remindClock{t: utc(2030, time.January, 2, 9, 0)}
	sch := schedule.New(w.e.db, w.e.s.access, schedule.Options{Clock: clock.now})
	if err := w.e.s.EnableReminders(sch); err != nil {
		t.Fatal(err)
	}
	w.e.s.access.OnArchiveChange(w.e.s.SyncSpaceReminders)
	w.e.s.access.OnMemberAdded(w.e.s.SyncSpaceMemberReminders)
	w.e.accounts.OnMemberAdded(w.e.s.SyncWorkspaceMemberReminders)
	w.e.accounts.OnTimeZoneChanged(w.e.s.SyncTimeZoneReminders)
	w.e.addMember(w.owner, w.shared, w.mallory, access.RoleEditor)
	f := &remindFX{world: w, clock: clock, sch: sch}
	rec := w.alice.do(http.MethodPost, appsPath(w.shared, ""), map[string]any{"name": "Dues", "definition": remindDefinition(reminders...)})
	expect(t, rec, http.StatusCreated)
	f.app = w.publish(w.alice, w.shared, decode[App](t, rec))
	return f
}

func (f *remindFX) zone(u *user, zone string) {
	f.e.t.Helper()
	expect(f.e.t, u.do(http.MethodPatch, "/api/me", map[string]any{"time_zone": zone}), http.StatusOK)
}

func (f *remindFX) insert(u *user, fields map[string]any) int64 {
	f.e.t.Helper()
	expect(f.e.t, u.do(http.MethodPost, appPath(f.shared, f.app.ID, "/actions"), action(f.app, "add", fields)), http.StatusOK)
	var id int64
	if err := f.e.db.QueryRow(`SELECT max(id) FROM app_rows WHERE app_id = ?`, f.app.ID).Scan(&id); err != nil {
		f.e.t.Fatal(err)
	}
	return id
}

func (f *remindFX) edit(u *user, id int64, fields map[string]any) {
	f.e.t.Helper()
	ctx := map[string]any{"id": id}
	for k, v := range fields {
		ctx[k] = v
	}
	expect(f.e.t, u.do(http.MethodPost, appPath(f.shared, f.app.ID, "/actions"), action(f.app, "edit", ctx)), http.StatusOK)
}

func (f *remindFX) remove(u *user, id int64) {
	f.e.t.Helper()
	expect(f.e.t, u.do(http.MethodPost, appPath(f.shared, f.app.ID, "/actions"), action(f.app, "remove", map[string]any{"id": id})), http.StatusOK)
}

func (f *remindFX) job(u *user, id int64, name string) (schedule.Job, bool) {
	f.e.t.Helper()
	j, err := f.sch.Find(context.Background(), f.e.db, u.id, KindRecordReminder, reminderKey(f.app.ID, id, name))
	if errors.Is(err, schedule.ErrNotFound) {
		return schedule.Job{}, false
	}
	if err != nil {
		f.e.t.Fatal(err)
	}
	return j, true
}

func (f *remindFX) wantJob(u *user, id int64, name string, at time.Time) {
	f.e.t.Helper()
	j, ok := f.job(u, id, name)
	if !ok || !j.RunAt.Equal(at) {
		f.e.t.Fatalf("job of record %d for user %d = %v (found %v), want it at %v", id, u.id, j.RunAt, ok, at)
	}
}

func (f *remindFX) wantNoJob(u *user, id int64, name string) {
	f.e.t.Helper()
	if j, ok := f.job(u, id, name); ok {
		f.e.t.Fatalf("user %d still has a job for record %d at %v", u.id, id, j.RunAt)
	}
}

func (f *remindFX) run(to time.Time) {
	f.e.t.Helper()
	f.clock.set(to)
	f.sch.RunDue(context.Background())
}

func (f *remindFX) notices(u *user) []string {
	f.e.t.Helper()
	rows, err := f.e.db.Query(`SELECT ref FROM notifications WHERE user_id = ? AND kind = ? ORDER BY id`, u.id, notifications.KindAppReminder)
	if err != nil {
		f.e.t.Fatal(err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			f.e.t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	return refs
}

func (f *remindFX) republish(def Definition) App {
	f.e.t.Helper()
	cur := f.view(f.alice, f.shared, f.app)
	rec := f.alice.do(http.MethodPatch, appPath(f.shared, f.app.ID, ""), map[string]any{"version": cur.Version, "definition": def})
	expect(f.e.t, rec, http.StatusOK)
	f.app = f.publish(f.alice, f.shared, decode[App](f.e.t, rec))
	return f.app
}

func (f *remindFX) text(u *user, id int64, name string) (notifications.ReminderText, bool) {
	f.e.t.Helper()
	got, ok, err := f.e.s.ReminderText(context.Background(), f.e.s.access, u.id, f.ws,
		notifications.Ref{Space: f.shared.ID, App: f.app.ID, Record: id, Reminder: name})
	if err != nil {
		f.e.t.Fatal(err)
	}
	return got, ok
}

func TestARecordRemindsItsCreatorOnceAtNineInTheirZone(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "Pay {{title}}"})
	f.zone(f.alice, "America/Sao_Paulo")
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})

	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 12, 0))
	j, _ := f.job(f.alice, id, "due")
	if j.WorkspaceID != f.ws || j.SpaceID != f.shared.ID {
		t.Fatalf("job = %+v", j)
	}
	if want := fmt.Sprintf(`{"app":%d,"row":%d,"name":"due"}`, f.app.ID, id); string(j.Payload) != want {
		t.Fatalf("payload = %s, want only IDs: %s", j.Payload, want)
	}
	for _, other := range []*user{f.owner, f.bob, f.mallory} {
		f.wantNoJob(other, id, "due")
	}

	f.run(utc(2030, time.January, 5, 11, 59))
	if n := len(f.notices(f.alice)); n != 0 {
		t.Fatalf("%d notices a minute early", n)
	}
	f.run(utc(2030, time.January, 5, 12, 0))
	refs := f.notices(f.alice)
	if want := fmt.Sprintf(`{"space":%d,"app":%d,"record":%d,"reminder":"due"}`, f.shared.ID, f.app.ID, id); len(refs) != 1 || refs[0] != want {
		t.Fatalf("notices = %v, want one with %s", refs, want)
	}
	f.run(utc(2030, time.January, 6, 12, 0))
	f.run(utc(2030, time.January, 9, 12, 0))
	if n := len(f.notices(f.alice)); n != 1 {
		t.Fatalf("%d notices after later runs, want it once", n)
	}
	for _, other := range []*user{f.owner, f.bob, f.mallory} {
		if n := len(f.notices(other)); n != 0 {
			t.Fatalf("user %d got %d reminders of alice's record", other.id, n)
		}
	}
}

func TestTheTimeFieldGivesTheHourAndAnEmptyOneFallsBackToAt(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Time: "starts", At: "07:15", Title: "{{title}}"})
	withTime := f.insert(f.alice, map[string]any{"title": "a", "due": "2030-01-05", "starts": "17:30"})
	without := f.insert(f.alice, map[string]any{"title": "b", "due": "2030-01-05"})
	f.wantJob(f.alice, withTime, "due", utc(2030, time.January, 5, 17, 30))
	f.wantJob(f.alice, without, "due", utc(2030, time.January, 5, 7, 15))

	plain := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
	id := plain.insert(plain.alice, map[string]any{"title": "c", "due": "2030-01-05"})
	plain.wantJob(plain.alice, id, "due", utc(2030, time.January, 5, 9, 0))
}

func TestChangingTheDateOrTheConditionMovesOrCancelsTheReminder(t *testing.T) {
	f := newRemindFX(t, dueReminder())
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05", "starts": "10:00"})
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 10, 0))
	before, _ := f.job(f.alice, id, "due")

	f.edit(f.alice, id, map[string]any{"due": "2030-01-08"})
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 8, 10, 0))
	after, _ := f.job(f.alice, id, "due")
	if after.ID != before.ID || after.Revision <= before.Revision {
		t.Fatalf("job before %+v, after %+v: want the same job, moved", before, after)
	}

	f.edit(f.alice, id, map[string]any{"status": "done"})
	f.wantNoJob(f.alice, id, "due")
	f.edit(f.alice, id, map[string]any{"status": "open"})
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 8, 10, 0))

	f.edit(f.alice, id, map[string]any{"starts": "11:45"})
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 8, 11, 45))

	f.run(utc(2030, time.January, 7, 10, 0))
	if n := len(f.notices(f.alice)); n != 0 {
		t.Fatalf("%d notices at the old date", n)
	}
	f.run(utc(2030, time.January, 8, 11, 45))
	if n := len(f.notices(f.alice)); n != 1 {
		t.Fatalf("%d notices at the new date, want 1", n)
	}
}

func TestDeletingARecordCancelsItsReminderAndRestoringItBringsItBack(t *testing.T) {
	f := newRemindFX(t, dueReminder())
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 9, 0))

	f.remove(f.alice, id)
	f.wantNoJob(f.alice, id, "due")
	f.run(utc(2030, time.January, 5, 9, 0))
	if n := len(f.notices(f.alice)); n != 0 {
		t.Fatalf("%d notices of a deleted record", n)
	}

	f.clock.set(utc(2030, time.January, 3, 9, 0))
	var revision int64
	if err := f.e.db.QueryRow(`SELECT revision FROM app_rows WHERE id = ?`, id).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	expect(t, f.restore(f.alice, f.shared, f.app, id, rev(revision)), http.StatusOK)
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 9, 0))
	f.run(utc(2030, time.January, 5, 9, 0))
	if n := len(f.notices(f.alice)); n != 1 {
		t.Fatalf("%d notices of a restored record, want 1", n)
	}
}

func TestAPastDateSchedulesNothing(t *testing.T) {
	f := newRemindFX(t, dueReminder())
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-01"})
	f.wantNoJob(f.alice, id, "due")
	today := f.insert(f.alice, map[string]any{"title": "today", "due": "2030-01-02", "starts": "09:00"})
	f.wantNoJob(f.alice, today, "due")
	f.edit(f.alice, id, map[string]any{"due": "2030-01-04"})
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 4, 9, 0))
	f.edit(f.alice, id, map[string]any{"due": "2029-12-31"})
	f.wantNoJob(f.alice, id, "due")
	f.edit(f.alice, id, map[string]any{"due": "2030-01-04"})
	f.edit(f.alice, id, map[string]any{"title": "rent again"})
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 4, 9, 0))
}

func TestAWhitespaceAroundAStoredDateOrTimeIsStillScheduled(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Time: "starts", Title: "{{title}}"})
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 9, 0))
	res, err := f.e.s.SystemWrite(context.Background(), SystemWrite{
		Actor:   SystemActor{Kind: SystemKindIntegration, ID: 1},
		SpaceID: f.shared.ID, AppID: f.app.ID, RecordID: id, Action: "payment.succeeded",
		Declared: []string{"due", "starts"}, Set: map[string]any{"due": "2030-01-08 ", "starts": "17:30 "},
	})
	if err != nil || !res.Changed {
		t.Fatalf("system write = %+v, %v", res, err)
	}
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 8, 17, 30))
}

func TestAPendingReminderPastItsTimeSurvivesTheAppMoving(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	other := f.e.createShared(f.owner, f.ws, "Elsewhere")
	f.e.addMember(f.owner, other, f.alice, access.RoleEditor)
	f.clock.set(utc(2030, time.January, 5, 9, 1))
	cur := f.view(f.alice, f.shared, f.app)
	rec := f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, "/move"), map[string]any{"version": cur.Version, "space_id": other.ID})
	expect(t, rec, http.StatusOK)
	j, ok := f.job(f.alice, id, "due")
	if !ok || j.SpaceID != other.ID || !j.RunAt.Equal(utc(2030, time.January, 5, 9, 0)) {
		t.Fatalf("job = %+v (found %v), want the same time in the new space", j, ok)
	}
	f.sch.RunDue(context.Background())
	if n := len(f.notices(f.alice)); n != 1 {
		t.Fatalf("%d notices, want the pending one", n)
	}
}

func TestAReleaseWithoutTheReminderMakesTheJobDoNothing(t *testing.T) {
	f := newRemindFX(t, dueReminder())
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 9, 0))

	f.republish(remindDefinition())
	f.run(utc(2030, time.January, 5, 9, 0))
	if n := len(f.notices(f.alice)); n != 0 {
		t.Fatalf("%d notices of a reminder the live release no longer has", n)
	}
}

func TestAReleaseThatChangesTheReminderMovesTheJobsOfExistingRecords(t *testing.T) {
	f := newRemindFX(t)
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	f.wantNoJob(f.alice, id, "due")

	f.republish(remindDefinition(Reminder{Name: "due", Date: "due", At: "08:00", Title: "{{title}}"}))
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 8, 0))

	f.republish(remindDefinition(Reminder{Name: "due", Date: "due", At: "18:00", Title: "{{title}}"}))
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 18, 0))

	rec := f.alice.do(http.MethodPost, appPath(f.shared, f.app.ID, "/rollback"), map[string]any{"version": f.view(f.alice, f.shared, f.app).Version, "release": 2})
	expect(t, rec, http.StatusOK)
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 8, 0))
}

func TestAnArchivedAppRemindsNoOneUntilItIsRestored(t *testing.T) {
	f := newRemindFX(t, dueReminder())
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	cur := f.view(f.alice, f.shared, f.app)
	expect(t, f.alice.do(http.MethodPatch, appPath(f.shared, f.app.ID, ""), map[string]any{"version": cur.Version, "archived": true}), http.StatusOK)
	f.wantNoJob(f.alice, id, "due")

	cur = f.view(f.alice, f.shared, f.app)
	expect(t, f.alice.do(http.MethodPatch, appPath(f.shared, f.app.ID, ""), map[string]any{"version": cur.Version, "archived": false}), http.StatusOK)
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 9, 0))
}

func TestAJobOfAnAppThatWasArchivedLaterIsANoOp(t *testing.T) {
	f := newRemindFX(t, dueReminder())
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	if _, err := f.e.db.Exec(`UPDATE apps SET archived_at = ? WHERE id = ?`, now(), f.app.ID); err != nil {
		t.Fatal(err)
	}
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 9, 0))
	f.run(utc(2030, time.January, 5, 9, 0))
	if n := len(f.notices(f.alice)); n != 0 {
		t.Fatalf("%d notices of an archived app", n)
	}
}

func TestChangingTheZoneMovesTheRemindersOfTheRecordsTheUserCreated(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
	mine := f.insert(f.alice, map[string]any{"title": "mine", "due": "2030-01-05"})
	theirs := f.insert(f.mallory, map[string]any{"title": "theirs", "due": "2030-01-05"})
	f.wantJob(f.alice, mine, "due", utc(2030, time.January, 5, 9, 0))

	f.zone(f.alice, "Asia/Tokyo")
	f.wantJob(f.alice, mine, "due", utc(2030, time.January, 5, 0, 0))
	f.wantJob(f.mallory, theirs, "due", utc(2030, time.January, 5, 9, 0))
}

func TestAJobThatFiresAfterAZoneChangeWithNoSyncStillTellsOnceOrMovesItself(t *testing.T) {
	t.Run("the new time has passed", func(t *testing.T) {
		f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
		f.insert(f.alice, map[string]any{"title": "mine", "due": "2030-01-05"})
		if _, err := f.e.db.Exec(`UPDATE users SET time_zone = 'Asia/Tokyo' WHERE id = ?`, f.alice.id); err != nil {
			t.Fatal(err)
		}
		f.run(utc(2030, time.January, 5, 9, 0))
		f.run(utc(2030, time.January, 6, 9, 0))
		if n := len(f.notices(f.alice)); n != 1 {
			t.Fatalf("%d notices, want the one that was due in the new zone 9 hours earlier", n)
		}
	})
	t.Run("the new time is still ahead", func(t *testing.T) {
		f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
		id := f.insert(f.alice, map[string]any{"title": "mine", "due": "2030-01-05"})
		if _, err := f.e.db.Exec(`UPDATE users SET time_zone = 'Pacific/Honolulu' WHERE id = ?`, f.alice.id); err != nil {
			t.Fatal(err)
		}
		f.run(utc(2030, time.January, 5, 9, 0))
		if n := len(f.notices(f.alice)); n != 0 {
			t.Fatalf("%d notices before the time in the person's zone", n)
		}
		f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 19, 0))
		f.run(utc(2030, time.January, 5, 19, 0))
		if n := len(f.notices(f.alice)); n != 1 {
			t.Fatalf("%d notices at the new time, want 1", n)
		}
	})
}

func TestASystemWriteMovesTheReminderToo(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	res, err := f.e.s.SystemWrite(context.Background(), SystemWrite{
		Actor:   SystemActor{Kind: SystemKindIntegration, ID: 1},
		SpaceID: f.shared.ID, AppID: f.app.ID, RecordID: id, Action: "payment.succeeded",
		Declared: []string{"due"}, Set: map[string]any{"due": "2030-01-09"},
	})
	if err != nil || !res.Changed {
		t.Fatalf("system write = %+v, %v", res, err)
	}
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 9, 9, 0))
}

func TestOnlyTheCreatorIsRemindedAndOnlyWhileTheyMayReadTheRecord(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "Pay {{title}}"})
	f.mustSetRoles(f.alice, f.shared, f.app,
		grant(f.alice.id, access.AppBuilder, access.AppPublisher, access.AppDataManager),
		grant(f.mallory.id, access.AppUser))
	hers := f.insert(f.alice, map[string]any{"title": "alice-secret", "due": "2030-01-05"})
	mine := f.insert(f.mallory, map[string]any{"title": "mallory-secret", "due": "2030-01-05"})

	f.wantJob(f.alice, hers, "due", utc(2030, time.January, 5, 9, 0))
	f.wantJob(f.mallory, mine, "due", utc(2030, time.January, 5, 9, 0))
	f.wantNoJob(f.mallory, hers, "due")
	f.wantNoJob(f.alice, mine, "due")

	if _, ok := f.text(f.mallory, hers, "due"); ok {
		t.Fatal("mallory, a user, reads the text of a reminder of alice's record")
	}
	if got, ok := f.text(f.mallory, mine, "due"); !ok || got.Title != "Pay mallory-secret" || got.App != "Dues" {
		t.Fatalf("text = %+v, %v", got, ok)
	}

	f.run(utc(2030, time.January, 5, 9, 0))
	if n := len(f.notices(f.alice)); n != 1 {
		t.Fatalf("alice has %d notices, want hers", n)
	}
	if n := len(f.notices(f.mallory)); n != 1 {
		t.Fatalf("mallory has %d notices, want hers", n)
	}
	if refs := f.notices(f.mallory); strings.Contains(refs[0], "secret") || !strings.Contains(refs[0], fmt.Sprintf(`"record":%d`, mine)) {
		t.Fatalf("mallory's notice = %s", refs[0])
	}
}

func TestACreatorWhoLostTheAppIsNotRemindedOfItsRecords(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "Pay {{title}}"})
	f.mustSetRoles(f.alice, f.shared, f.app,
		grant(f.alice.id, access.AppBuilder, access.AppPublisher, access.AppDataManager),
		grant(f.mallory.id, access.AppUser))
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 9, 0))
	if got, ok := f.text(f.alice, id, "due"); !ok || got.Title != "Pay rent" {
		t.Fatalf("text before = %+v, %v", got, ok)
	}

	f.mustSetRoles(f.alice, f.shared, f.app, grant(f.alice.id, access.AppBuilder), grant(f.mallory.id, access.AppUser))
	if _, ok := f.text(f.alice, id, "due"); ok {
		t.Fatal("the text of a reminder is shown to someone who can't open the app")
	}
	f.run(utc(2030, time.January, 5, 9, 0))
	if n := len(f.notices(f.alice)); n != 0 {
		t.Fatalf("%d notices for someone who lost the app", n)
	}
}

func TestACreatorWhoLeftTheSpaceIsNotReminded(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "Pay {{title}}"})
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	expect(t, f.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", f.ws, f.shared.ID, f.alice.id), nil), http.StatusNoContent)

	f.run(utc(2030, time.January, 5, 9, 0))
	if n := len(f.notices(f.alice)); n != 0 {
		t.Fatalf("%d notices for a member who left", n)
	}
	if _, ok := f.text(f.alice, id, "due"); ok {
		t.Fatal("the text is shown to a member who left")
	}
}

func TestACreatorWhoIsAddedBackIsRemindedAgain(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "Pay {{title}}"})
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	expect(t, f.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", f.ws, f.shared.ID, f.alice.id), nil), http.StatusNoContent)
	f.e.addMember(f.owner, f.shared, f.alice, access.RoleEditor)
	f.wantJob(f.alice, id, "due", utc(2030, time.January, 5, 9, 0))
	f.run(utc(2030, time.January, 5, 9, 0))
	if n := len(f.notices(f.alice)); n != 1 {
		t.Fatalf("%d notices", n)
	}
}

func TestTheTextOfAReminderFollowsTheRecordAndNeverOutlivesIt(t *testing.T) {
	f := newRemindFX(t, dueReminder())
	id := f.insert(f.alice, map[string]any{"title": "rent", "due": "2030-01-05"})
	f.run(utc(2030, time.January, 5, 9, 0))
	if got, ok := f.text(f.alice, id, "due"); !ok || got.Title != "Pay rent" || got.App != "Dues" {
		t.Fatalf("text = %+v, %v", got, ok)
	}
	f.edit(f.alice, id, map[string]any{"title": "the rent"})
	if got, _ := f.text(f.alice, id, "due"); got.Title != "Pay the rent" {
		t.Fatalf("text after an edit = %+v", got)
	}
	if _, ok := f.text(f.bob, id, "due"); ok {
		t.Fatal("bob reads the text of alice's reminder")
	}
	if _, ok := f.text(f.alice, id, "gone"); ok {
		t.Fatal("the text of a reminder the app doesn't have")
	}
	f.edit(f.alice, id, map[string]any{"status": "done"})
	if _, ok := f.text(f.alice, id, "due"); ok {
		t.Fatal("the text is shown once the condition no longer holds")
	}
	f.edit(f.alice, id, map[string]any{"status": "open"})
	f.remove(f.alice, id)
	if _, ok := f.text(f.alice, id, "due"); ok {
		t.Fatal("the text of a deleted record is shown")
	}
}

func TestATitleThatComesOutEmptyFallsBackToTheAppsName(t *testing.T) {
	f := newRemindFX(t, Reminder{Name: "due", Date: "due", Title: "{{title}}"})
	id := f.insert(f.alice, map[string]any{"title": "x", "due": "2030-01-05"})
	if _, err := f.e.db.Exec(`UPDATE app_rows SET data = json_set(data, '$.title', '') WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if got, ok := f.text(f.alice, id, "due"); !ok || got.Title != "Dues" {
		t.Fatalf("text = %+v, %v", got, ok)
	}
}

func TestRemindersAreCheckedWithTheDefinition(t *testing.T) {
	good := Reminder{Name: "due", Date: "due", Time: "starts", At: "08:30", Title: "Pay {{title}} {{id}}", Where: &Rule{Field: "status", Op: OpEq, Value: "open"}}
	def := remindDefinition(good)
	checked, err := def.Check()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(checked.Requires, ","), RemindersCapability) {
		t.Fatalf("requires = %v", checked.Requires)
	}
	if plain, err := remindDefinition().Check(); err != nil || strings.Contains(strings.Join(plain.Requires, ","), RemindersCapability) {
		t.Fatalf("an app with no reminders requires %v, %v", plain.Requires, err)
	}

	bad := map[string]Reminder{
		"a date that isn't one":      {Name: "due", Date: "title", Title: "x"},
		"a date that isn't declared": {Name: "due", Date: "nope", Title: "x"},
		"a time that isn't one":      {Name: "due", Date: "due", Time: "due", Title: "x"},
		"a time that isn't declared": {Name: "due", Date: "due", Time: "nope", Title: "x"},
		"an hour of 25":              {Name: "due", Date: "due", At: "25:00", Title: "x"},
		"an hour with no minutes":    {Name: "due", Date: "due", At: "9", Title: "x"},
		"no title":                   {Name: "due", Date: "due"},
		"a hole that isn't a field":  {Name: "due", Date: "due", Title: "{{nope}}"},
		"an unclosed hole":           {Name: "due", Date: "due", Title: "{{title"},
		"a long title":               {Name: "due", Date: "due", Title: strings.Repeat("x", maxReminderTitleLen+1)},
		"a bad name":                 {Name: "Due!", Date: "due", Title: "x"},
		"a rule on a missing field":  {Name: "due", Date: "due", Title: "x", Where: &Rule{Field: "nope", Op: OpEq, Value: "x"}},
	}
	for what, r := range bad {
		if _, err := remindDefinition(r).Check(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", what, err)
		}
	}
	twice := remindDefinition(good, good)
	if _, err := twice.Check(); !errors.Is(err, ErrInvalid) {
		t.Errorf("a name declared twice: %v", err)
	}
	many := remindDefinition()
	for i := range maxReminders + 1 {
		many.Reminders = append(many.Reminders, Reminder{Name: fmt.Sprintf("r%d", i), Date: "due", Title: "x"})
	}
	if _, err := many.Check(); !errors.Is(err, ErrInvalid) {
		t.Errorf("%d reminders: %v", len(many.Reminders), err)
	}
	old := remindDefinition(good)
	old.DefinitionVersion = 2
	if _, err := old.Check(); !errors.Is(err, ErrInvalid) {
		t.Errorf("version 2 with reminders: %v", err)
	}

	var strict Definition
	if err := json.Unmarshal([]byte(`{"reminders":[{"name":"due","date":"due","title":"x","repeat":"monthly"}]}`), &strict); err == nil {
		t.Error("a reminder with a field the server doesn't know was accepted")
	}
}

func TestAHandlerGivenAMalformedPayloadFailsForGood(t *testing.T) {
	f := newRemindFX(t, dueReminder())
	err := f.e.s.remindRecord(context.Background(), schedule.Job{Payload: json.RawMessage(`{`)})
	if err == nil {
		t.Fatal("a payload that isn't JSON was accepted")
	}
}
