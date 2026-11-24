package boards

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/schedule"
)

// fakeClock is the scheduler's clock, which the tests move.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func at(day string, hour int) time.Time {
	d, err := time.Parse(dateLayout, day)
	if err != nil {
		panic(err)
	}
	return d.Add(time.Duration(hour) * time.Hour)
}

// remindWorld is a world whose boards schedule reminders on a scheduler with a fake clock, which
// starts on 2030-01-02 at 09:00 UTC. Its worker isn't started: a test runs the due jobs itself.
type remindWorld struct {
	*world
	clock *fakeClock
	sch   *schedule.Service
}

func newRemindWorld(t *testing.T) *remindWorld {
	t.Helper()
	w := newWorld(t)
	clock := &fakeClock{t: at("2030-01-02", 9)}
	sch := schedule.New(w.e.db, w.e.s.access, schedule.Options{Clock: clock.now})
	if err := w.e.s.EnableReminders(sch); err != nil {
		t.Fatal(err)
	}
	followMembers(w.e)
	return &remindWorld{world: w, clock: clock, sch: sch}
}

// followMembers has the reminders follow the people added to a space or a workspace, as the server
// does when it has a scheduler.
func followMembers(e *env) {
	e.s.access.OnMemberAdded(e.s.SyncSpaceMember)
	e.accounts.OnMemberAdded(e.s.SyncWorkspaceMember)
}

// card makes a card on the team board as the owner, with the given fields.
func (w *remindWorld) newCard(fields map[string]any) Card {
	w.e.t.Helper()
	if fields["title"] == nil {
		fields["title"] = "Reminded card"
	}
	return w.createCard(w.alice, w.shared, w.team, fields)
}

func (w *remindWorld) patch(c Card, fields map[string]any) Card {
	w.e.t.Helper()
	fields["version"] = c.Version
	rec := w.alice.do(http.MethodPatch, cardPath(w.shared, c.BoardID, c.ID, ""), fields)
	expect(w.e.t, rec, http.StatusOK)
	return decode[Card](w.e.t, rec)
}

func (w *remindWorld) post(c Card, action string, fields map[string]any) Card {
	w.e.t.Helper()
	fields["version"] = c.Version
	rec := w.alice.do(http.MethodPost, cardPath(w.shared, c.BoardID, c.ID, "/"+action), fields)
	expect(w.e.t, rec, http.StatusOK)
	return decode[Card](w.e.t, rec)
}

func asUser(id int64) map[string]any { return map[string]any{"kind": "user", "id": id} }

func (w *remindWorld) run(to time.Time) {
	w.e.t.Helper()
	w.clock.set(to)
	w.sch.RunDue(context.Background())
}

// jobs says when the reminder and the overdue notice of c are due for u: zero times for none.
func (w *remindWorld) jobs(u *user, c Card) (remind, overdue time.Time) {
	w.e.t.Helper()
	find := func(k schedule.Kind) time.Time {
		j, err := w.sch.Find(context.Background(), w.e.db, u.id, k, reminderKey(c.ID))
		if err != nil {
			return time.Time{}
		}
		return j.RunAt
	}
	return find(KindDueReminder), find(KindOverdue)
}

func (w *remindWorld) notices(u *user, kind string) int {
	w.e.t.Helper()
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM notifications WHERE user_id = ? AND kind = ?`, u.id, kind).Scan(&n); err != nil {
		w.e.t.Fatal(err)
	}
	return n
}

func wantJobs(t *testing.T, w *remindWorld, u *user, c Card, remind, overdue time.Time) {
	t.Helper()
	gotRemind, gotOverdue := w.jobs(u, c)
	if !gotRemind.Equal(remind) || !gotOverdue.Equal(overdue) {
		t.Fatalf("jobs of card %d for user %d: reminder %v, overdue %v; want %v and %v", c.ID, u.id, gotRemind, gotOverdue, remind, overdue)
	}
}

func TestReminderThenOverdueForTheAssignee(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))

	// Nothing before the morning of the due day.
	w.run(at("2030-01-10", 7))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 0 {
		t.Fatalf("a reminder before its time: %d", n)
	}
	// Once on the morning, however often the worker looks.
	w.run(at("2030-01-10", 8))
	w.run(at("2030-01-10", 20))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("reminders on the day = %d, want 1", n)
	}
	if n := w.notices(w.alice, notifications.KindCardOverdue); n != 0 {
		t.Fatalf("overdue on the day = %d, want 0", n)
	}
	// And the overdue one the morning after.
	w.run(at("2030-01-11", 8))
	w.run(at("2030-01-12", 8))
	if n := w.notices(w.alice, notifications.KindCardOverdue); n != 1 {
		t.Fatalf("overdue notices = %d, want 1", n)
	}
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("reminders after overdue = %d, want 1", n)
	}
	// Nobody else was told.
	for _, u := range []*user{w.owner, w.bob, w.mallory} {
		if n := w.notices(u, notifications.KindCardDue) + w.notices(u, notifications.KindCardOverdue); n != 0 {
			t.Fatalf("user %d was told of alice's card: %d", u.id, n)
		}
	}
}

func TestReminderIsOnceAcrossRetriesAndEdits(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	w.run(at("2030-01-10", 8))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("reminders = %d, want 1", n)
	}

	// The same occurrence handled again (a worker that died before it noted the job as done).
	job, err := w.sch.Find(context.Background(), w.e.db, w.alice.id, KindDueReminder, reminderKey(c.ID))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := w.e.s.remind(notifications.KindCardDue)(context.Background(), job); err != nil {
			t.Fatal(err)
		}
	}
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("reminders after the handler ran again = %d, want 1", n)
	}

	// Edits that leave the date and the assignee alone don't arm it again, moving the card about
	// the board included.
	c = w.patch(c, map[string]any{"title": "Renamed", "labels": []string{"a"}})
	c = w.post(c, "move", map[string]any{"column": "doing"})
	w.run(at("2030-01-10", 9))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("reminders after edits = %d, want 1", n)
	}
	// Its finished job is not made again for a time that has passed.
	if after, err := w.sch.Find(context.Background(), w.e.db, w.alice.id, KindDueReminder, reminderKey(c.ID)); err == nil && after.State != schedule.StateDone {
		t.Fatalf("the finished reminder was scheduled again: %+v", after)
	}
}

func TestReminderFollowsTheDueDate(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{}) // no due date: none

	c = w.patch(c, map[string]any{"due": "2030-01-10"})
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))
	before, _ := w.sch.Find(context.Background(), w.e.db, w.alice.id, KindDueReminder, reminderKey(c.ID))

	// Moved later: the old date passes with no word, the new one is the one.
	c = w.patch(c, map[string]any{"due": "2030-01-20"})
	wantJobs(t, w, w.alice, c, at("2030-01-20", 8), at("2030-01-21", 8))
	after, _ := w.sch.Find(context.Background(), w.e.db, w.alice.id, KindDueReminder, reminderKey(c.ID))
	if after.Occurrence() == before.Occurrence() {
		t.Fatal("a moved date is the same occurrence")
	}
	w.run(at("2030-01-15", 0))
	if n := w.notices(w.alice, notifications.KindCardDue) + w.notices(w.alice, notifications.KindCardOverdue); n != 0 {
		t.Fatalf("notified of the old date: %d", n)
	}
	w.run(at("2030-01-20", 8))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("reminders on the new date = %d, want 1", n)
	}

	// Moved again after its reminder was sent: a new date is a new reminder.
	c = w.patch(c, map[string]any{"due": "2030-02-01"})
	w.run(at("2030-02-01", 8))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 2 {
		t.Fatalf("reminders after moving it again = %d, want 2", n)
	}

	// Cleared: no more.
	c = w.patch(c, map[string]any{"due": nil})
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{})
	w.run(at("2030-02-05", 8))
	if n := w.notices(w.alice, notifications.KindCardOverdue); n != 0 {
		t.Fatalf("overdue notices for a card with no date: %d", n)
	}
}

func TestReminderFollowsTheAssignee(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))

	// Reassigned: the first assignee's go, the new one's are made.
	c = w.patch(c, map[string]any{"assignee": asUser(w.owner.id)})
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{})
	wantJobs(t, w, w.owner, c, at("2030-01-10", 8), at("2030-01-11", 8))
	w.run(at("2030-01-10", 8))
	if w.notices(w.alice, notifications.KindCardDue) != 0 || w.notices(w.owner, notifications.KindCardDue) != 1 {
		t.Fatal("the reminder went to the wrong person")
	}

	// The agent: no reminder for anyone.
	c = w.patch(c, map[string]any{"assignee": map[string]any{"kind": "agent"}})
	wantJobs(t, w, w.owner, c, time.Time{}, time.Time{})
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{})

	// A card made for the agent, and one for nobody: none.
	for _, fields := range []map[string]any{
		{"due": "2030-03-01", "assignee": map[string]any{"kind": "agent"}},
		{"due": "2030-03-01"},
	} {
		n := w.newCard(fields)
		for _, u := range []*user{w.alice, w.owner, w.bob} {
			wantJobs(t, w, u, n, time.Time{}, time.Time{})
		}
	}

	// Unassigned: none.
	c = w.patch(c, map[string]any{"assignee": asUser(w.alice.id), "due": "2030-04-01"})
	wantJobs(t, w, w.alice, c, at("2030-04-01", 8), at("2030-04-02", 8))
	c = w.patch(c, map[string]any{"assignee": nil})
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{})
}

func TestNoReminderForFinishedCards(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})

	// Done: cancelled. Back on the board: made again, for a date still ahead.
	c = w.post(c, "move", map[string]any{"column": "done"})
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{})
	c = w.post(c, "move", map[string]any{"column": "doing"})
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))

	// Archived: cancelled; restored: back.
	c = w.post(c, "archive", map[string]any{})
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{})
	c = w.post(c, "restore", map[string]any{})
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))

	// A card made done, or finished after the reminder was sent but before the overdue notice: no
	// overdue notice.
	made := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id), "column": "done"})
	wantJobs(t, w, w.alice, made, time.Time{}, time.Time{})
	w.run(at("2030-01-10", 8))
	c = w.post(c, "move", map[string]any{"column": "done"})
	w.run(at("2030-01-11", 8))
	if n := w.notices(w.alice, notifications.KindCardOverdue); n != 0 {
		t.Fatalf("overdue notices for a finished card: %d", n)
	}
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("reminders = %d, want the one sent before it was finished", n)
	}
	// Finishing it again changes nothing for a date that has passed.
	c = w.post(c, "move", map[string]any{"column": "review"})
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{})
}

func TestNoReminderForATimeThatHasPassed(t *testing.T) {
	w := newRemindWorld(t) // 2030-01-02, 09:00

	// Due today, after the morning: no reminder, but the overdue notice is still ahead.
	c := w.newCard(map[string]any{"due": "2030-01-02", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, c, time.Time{}, at("2030-01-03", 8))

	// Due in the past: nothing to catch up on.
	c = w.newCard(map[string]any{"due": "2029-12-31", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{})
	w.run(at("2030-01-05", 8))
	if n := w.notices(w.alice, notifications.KindCardOverdue); n != 1 {
		t.Fatalf("overdue notices = %d, want only the one for the card due today", n)
	}
}

func TestReminderMissedWhileTheServerWasAway(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	// The worker looks for the first time after the day: the reminder would be false, the overdue
	// notice is the true one.
	w.run(at("2030-01-12", 9))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 0 {
		t.Fatalf("a reminder after its day: %d", n)
	}
	if n := w.notices(w.alice, notifications.KindCardOverdue); n != 1 {
		t.Fatalf("overdue notices = %d, want 1", n)
	}
	_ = c

	// A restart on the day itself, late: still sent, once.
	d := w.newCard(map[string]any{"due": "2030-01-20", "assignee": asUser(w.alice.id)})
	w.run(at("2030-01-20", 23))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("a late reminder on the day = %d, want 1", n)
	}
	_ = d
}

func TestNoReminderAfterTheAssigneeLostAccess(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	kept := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.owner.id)})

	// Alice leaves the space (not the workspace): her jobs go with her access, in the same commit,
	// and no longer hold places of her bound.
	expect(t, w.owner.do(http.MethodDelete,
		fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", w.ws, w.shared.ID, w.alice.id), nil), http.StatusNoContent)
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{})
	w.run(at("2030-01-10", 8))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 0 {
		t.Fatalf("a reminder for someone who can't read the space: %d", n)
	}
	// The owner's, on the same morning, were sent.
	if n := w.notices(w.owner, notifications.KindCardDue); n != 1 {
		t.Fatalf("the owner's reminders = %d, want 1", n)
	}
	_ = kept
}

func TestReminderChecksTheCardWhenItRuns(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})

	// Changes that bypass the card's code (a restore of an older database, a bug) leave jobs that
	// no longer fit the card: they make nothing.
	if _, err := w.e.db.Exec(`UPDATE cards SET due = '2030-01-15' WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	other := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	if _, err := w.e.db.Exec(`UPDATE cards SET assignee_id = ? WHERE id = ?`, w.owner.id, other.ID); err != nil {
		t.Fatal(err)
	}
	gone := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	if _, err := w.e.db.Exec(`DELETE FROM cards WHERE id = ?`, gone.ID); err != nil {
		t.Fatal(err)
	}
	w.run(at("2030-01-10", 8))
	if n := w.notices(w.alice, notifications.KindCardDue) + w.notices(w.owner, notifications.KindCardDue); n != 0 {
		t.Fatalf("notifications for cards that no longer fit: %d", n)
	}

	// An archived board's cards are left alone.
	archived := w.newCard(map[string]any{"due": "2030-02-10", "assignee": asUser(w.alice.id)})
	rec := w.alice.do(http.MethodPatch, boardPath(w.shared, w.team.ID, ""), map[string]any{"version": w.boardVersion(w.team.ID), "archived": true})
	expect(t, rec, http.StatusOK)
	w.run(at("2030-02-10", 8))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 0 {
		t.Fatalf("notifications for a card of an archived board: %d", n)
	}
	_ = archived
}

func (w *remindWorld) boardVersion(id int64) int64 {
	w.e.t.Helper()
	var v int64
	if err := w.e.db.QueryRow(`SELECT version FROM boards WHERE id = ?`, id).Scan(&v); err != nil {
		w.e.t.Fatal(err)
	}
	return v
}

func TestRejectedChangeSchedulesNothing(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"title": "plain"})

	// A change made against an old version is refused whole.
	rec := w.alice.do(http.MethodPatch, cardPath(w.shared, c.BoardID, c.ID, ""),
		map[string]any{"version": 99, "due": "2030-01-10", "assignee": asUser(w.alice.id)})
	expect(t, rec, http.StatusConflict)
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{})

	// So is a card made with a bad field, and a viewer's attempt.
	expect(t, w.alice.do(http.MethodPost, boardPath(w.shared, w.team.ID, "/cards"),
		map[string]any{"title": "x", "due": "tomorrow", "assignee": asUser(w.alice.id)}), http.StatusBadRequest)
	expect(t, w.bob.do(http.MethodPatch, cardPath(w.shared, c.BoardID, c.ID, ""),
		map[string]any{"version": c.Version, "due": "2030-01-10", "assignee": asUser(w.bob.id)}), http.StatusForbidden)
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM scheduled_jobs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("scheduled jobs = %d, %v; want none", n, err)
	}
}

func TestRemindersOffWithoutAScheduler(t *testing.T) {
	w := newWorld(t) // no scheduler
	c := w.createCard(w.alice, w.shared, w.team, map[string]any{"title": "x", "due": "2030-01-10", "assignee": asUser(w.alice.id)})
	if c.Due == nil {
		t.Fatal("the card has no date")
	}
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM scheduled_jobs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("scheduled jobs = %d, %v", n, err)
	}
}

// A reminder that is ready but has not run yet (the worker is behind, or the server was away) is
// not taken away by a change that has nothing to do with it.
func TestALabelsOnlyChangeKeepsAReadyReminder(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	w.clock.set(at("2030-01-10", 9)) // the morning has come; the worker has not run
	c = w.patch(c, map[string]any{"labels": []string{"money"}})
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))
	w.run(at("2030-01-10", 10))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("the reminder that was ready = %d notices, want 1", n)
	}
	// A date that moves to one already past still drops them: there is nothing to catch up on.
	c = w.patch(c, map[string]any{"due": "2030-01-05"})
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{})
}

func TestTheLastDayTheSchedulerHoldsStillSavesTheCard(t *testing.T) {
	w := newRemindWorld(t)
	// The overdue notice of this day would be in the year 9000, which no job can be.
	c := w.newCard(map[string]any{"due": "8999-12-31", "assignee": asUser(w.alice.id)})
	if c.Due == nil || *c.Due != "8999-12-31" {
		t.Fatalf("the card's date = %v", c.Due)
	}
	wantJobs(t, w, w.alice, c, at("8999-12-31", 8), time.Time{})
	// A patch to it, and from it, are no different.
	c = w.patch(c, map[string]any{"labels": []string{"far"}})
	wantJobs(t, w, w.alice, c, at("8999-12-31", 8), time.Time{})
	c = w.patch(c, map[string]any{"due": "2030-01-10"})
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))
}

// remindHooks is a world whose reminders' handlers run through hook first, as the same worker
// would call them: what a test changes there is changed after the job was claimed.
func newHookedWorld(t *testing.T, hook func(schedule.Job)) *remindWorld {
	t.Helper()
	w := newWorld(t)
	clock := &fakeClock{t: at("2030-01-02", 9)}
	sch := schedule.New(w.e.db, w.e.s.access, schedule.Options{Clock: clock.now})
	for kind, notice := range map[schedule.Kind]string{KindDueReminder: notifications.KindCardDue, KindOverdue: notifications.KindCardOverdue} {
		real := w.e.s.remind(notice)
		if err := sch.Register(kind, func(ctx context.Context, job schedule.Job) error {
			hook(job)
			return real(ctx, job)
		}); err != nil {
			t.Fatal(err)
		}
	}
	w.e.s.WithScheduler(sch)
	followMembers(w.e)
	return &remindWorld{world: w, clock: clock, sch: sch}
}

func TestACancelledClaimMakesNoNotice(t *testing.T) {
	var w *remindWorld
	cancel := true
	w = newHookedWorld(t, func(job schedule.Job) {
		if cancel { // the card was reassigned and the job cancelled, after the worker took it
			if _, err := w.sch.Cancel(context.Background(), w.e.db, job.UserID, job.Kind, job.DedupeKey); err != nil {
				t.Error(err)
			}
		}
	})
	w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	w.run(at("2030-01-10", 8))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 0 {
		t.Fatalf("a job that was cancelled made %d notices", n)
	}
}

func TestANoticeIsOncePerPersonCardKindAndDate(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	w.run(at("2030-01-10", 8))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("notices = %d, want 1", n)
	}
	// The clock stepped back and the job ran again as a new occurrence (a revision later): the person
	// is not told twice of the same date.
	if _, err := w.e.db.Exec(`UPDATE scheduled_jobs SET state = 'pending', revision = revision + 1, lease_until = NULL
		WHERE dedupe_key = ?`, reminderKey(c.ID)); err != nil {
		t.Fatal(err)
	}
	w.run(at("2030-01-10", 9))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("notices after the job ran again = %d, want 1", n)
	}
	if n := w.notices(w.alice, notifications.KindCardOverdue); n != 0 {
		t.Fatalf("overdue notices = %d, want 0", n)
	}
	// A new date is news again.
	c = w.patch(c, map[string]any{"due": "2030-01-20"})
	w.run(at("2030-01-20", 8))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 2 {
		t.Fatalf("notices after a new date = %d, want 2", n)
	}
}

func TestAReminderChecksAccessInItsOwnTransaction(t *testing.T) {
	var w *remindWorld
	w = newHookedWorld(t, func(schedule.Job) {
		// After the worker's own check and before the handler's: access ends, and the schema's
		// cleanup of the person's jobs is out of the way so the handler is the one that must refuse.
		for _, q := range []string{`DROP TRIGGER space_members_drop_jobs`,
			fmt.Sprintf(`DELETE FROM space_members WHERE space_id = %d AND user_id = %d`, w.shared.ID, w.alice.id)} {
			if _, err := w.e.db.Exec(q); err != nil {
				t.Error(err)
			}
		}
	})
	w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	w.run(at("2030-01-10", 8))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 0 {
		t.Fatalf("a reminder for a person who lost access = %d", n)
	}
}

func TestArchivingABoardOrASpaceTakesTheRemindersAndRestoringGivesThemBack(t *testing.T) {
	w := newRemindWorld(t)
	w.e.s.access.OnArchiveChange(w.e.s.SyncSpace)
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))

	patchBoard := func(archived bool) {
		w.e.t.Helper()
		expect(w.e.t, w.alice.do(http.MethodPatch, boardPath(w.shared, w.team.ID, ""),
			map[string]any{"version": w.boardVersion(w.team.ID), "archived": archived}), http.StatusOK)
	}
	patchBoard(true)
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{})
	patchBoard(false)
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))

	expect(t, w.owner.do(http.MethodPatch, spacePath(w.shared, ""), map[string]bool{"archived": true}), http.StatusOK)
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{})
	expect(t, w.owner.do(http.MethodPatch, spacePath(w.shared, ""), map[string]bool{"archived": false}), http.StatusOK)
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))
}

func TestLeavingTheWorkspaceTakesTheRemindersAndTheirPlaceInTheBound(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", w.ws, w.alice.id), nil), http.StatusNoContent)
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{})
}

func (w *remindWorld) removeFromSpace(u *user) {
	w.e.t.Helper()
	expect(w.e.t, w.owner.do(http.MethodDelete, spacePath(w.shared, fmt.Sprintf("/members/%d", u.id)), nil), http.StatusNoContent)
}

func TestAddingAPersonToASpaceBringsTheirRemindersBack(t *testing.T) {
	w := newRemindWorld(t)
	mine := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	bobs := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.bob.id)})
	nobodys := w.newCard(map[string]any{"due": "2030-01-10"})
	finished := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id), "column": "done"})
	past := w.newCard(map[string]any{"due": "2030-01-01", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, mine, at("2030-01-10", 8), at("2030-01-11", 8))
	wantJobs(t, w, w.bob, bobs, at("2030-01-10", 8), at("2030-01-11", 8))

	w.removeFromSpace(w.alice)
	wantJobs(t, w, w.alice, mine, time.Time{}, time.Time{})
	wantJobs(t, w, w.bob, bobs, at("2030-01-10", 8), at("2030-01-11", 8)) // not theirs to lose

	var bobRevision int64
	bobJob := func() {
		t.Helper()
		if err := w.e.db.QueryRow(`SELECT revision FROM scheduled_jobs WHERE user_id = ? AND dedupe_key = ? AND kind = ?`,
			w.bob.id, reminderKey(bobs.ID), KindDueReminder).Scan(&bobRevision); err != nil {
			t.Fatal(err)
		}
	}
	bobJob()
	before := bobRevision

	w.e.addMember(w.owner, w.shared, w.alice, access.RoleEditor)
	wantJobs(t, w, w.alice, mine, at("2030-01-10", 8), at("2030-01-11", 8))
	// Only for the person who was added, and only for a card that can still have them.
	wantJobs(t, w, w.alice, bobs, time.Time{}, time.Time{})
	wantJobs(t, w, w.bob, bobs, at("2030-01-10", 8), at("2030-01-11", 8))
	wantJobs(t, w, w.alice, nobodys, time.Time{}, time.Time{})
	wantJobs(t, w, w.alice, finished, time.Time{}, time.Time{})
	wantJobs(t, w, w.alice, past, time.Time{}, time.Time{})
	if bobJob(); bobRevision != before {
		t.Fatalf("another person's job was rewritten: revision %d, was %d", bobRevision, before)
	}

	w.run(at("2030-01-10", 8))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("notices after being added back = %d, want 1", n)
	}
	w.run(at("2030-01-11", 8))
	if n := w.notices(w.alice, notifications.KindCardOverdue); n != 1 {
		t.Fatalf("overdue notices after being added back = %d, want 1", n)
	}
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("notices = %d, want still 1", n)
	}
}

func TestAddingBackAPersonAfterTheirReminderWentOutDoesNotRemindThemTwice(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	w.run(at("2030-01-10", 8))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("notices = %d, want 1", n)
	}
	w.removeFromSpace(w.alice)
	w.clock.set(at("2030-01-10", 9))
	w.e.addMember(w.owner, w.shared, w.alice, access.RoleEditor)
	// The morning is past, so no reminder is made again; the overdue notice is still ahead.
	wantJobs(t, w, w.alice, c, time.Time{}, at("2030-01-11", 8))
	w.run(at("2030-01-11", 8))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("notices = %d, want 1", n)
	}
	if n := w.notices(w.alice, notifications.KindCardOverdue); n != 1 {
		t.Fatalf("overdue notices = %d, want 1", n)
	}
}

func TestAddingAPersonBackKeepsTheJobsThatAreAsTheyShouldBe(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	// A job of theirs that is as it should be, as a restored backup or a second hook run leaves it.
	var revision int64
	if err := w.e.db.QueryRow(`SELECT revision FROM scheduled_jobs WHERE user_id = ? AND dedupe_key = ? AND kind = ?`,
		w.alice.id, reminderKey(c.ID), KindDueReminder).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	tx, err := w.e.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := w.e.s.SyncSpaceMember(context.Background(), tx, w.shared.ID, w.alice.id); err != nil {
			t.Fatal(err)
		}
		if err := w.e.s.SyncWorkspaceMember(context.Background(), tx, w.ws, w.alice.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err := w.e.db.QueryRow(`SELECT revision FROM scheduled_jobs WHERE user_id = ? AND dedupe_key = ? AND kind = ?`,
		w.alice.id, reminderKey(c.ID), KindDueReminder).Scan(&after); err != nil || after != revision {
		t.Fatalf("revision = %d, %v; want %d: a job that is as it should be stays", after, err, revision)
	}
}

func TestAddingAPersonBackToTheWorkspaceBringsBackTheRemindersOfTheirPrivateSpace(t *testing.T) {
	w := newRemindWorld(t)
	mine := w.createCard(w.alice, w.alicePrivate, w.alicePersonal,
		map[string]any{"title": "Mine", "due": "2030-01-10", "assignee": asUser(w.alice.id)})
	shared := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, mine, at("2030-01-10", 8), at("2030-01-11", 8))

	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", w.ws, w.alice.id), nil), http.StatusNoContent)
	wantJobs(t, w, w.alice, mine, time.Time{}, time.Time{})
	wantJobs(t, w, w.alice, shared, time.Time{}, time.Time{})

	// Invited again, and accepts as the same person.
	rec := w.owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", w.ws),
		map[string]string{"email": "alice@example.com", "role": string(auth.RoleMember)})
	expect(t, rec, http.StatusCreated)
	token := decode[struct{ Token string }](t, rec).Token
	expect(t, w.alice.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": token}), http.StatusOK)

	wantJobs(t, w, w.alice, mine, at("2030-01-10", 8), at("2030-01-11", 8))
	// A shared space is theirs again only when they are added to it.
	wantJobs(t, w, w.alice, shared, time.Time{}, time.Time{})
	w.e.addMember(w.owner, w.shared, w.alice, access.RoleEditor)
	wantJobs(t, w, w.alice, shared, at("2030-01-10", 8), at("2030-01-11", 8))

	w.run(at("2030-01-10", 8))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 2 {
		t.Fatalf("notices = %d, want 2: one for each card", n)
	}
}

func TestCardsDatedBeforeRemindersExistGetTheirsAtStart(t *testing.T) {
	w := newWorld(t) // no scheduler yet: these cards are as an older server made them
	mk := func(fields map[string]any) Card { return w.createCard(w.alice, w.shared, w.team, fields) }
	future := mk(map[string]any{"title": "future", "due": "2030-01-10", "assignee": asUser(w.alice.id)})
	today := mk(map[string]any{"title": "today", "due": "2030-01-02", "assignee": asUser(w.alice.id)})
	yesterday := mk(map[string]any{"title": "yesterday", "due": "2030-01-01", "assignee": asUser(w.alice.id)})
	longAgo := mk(map[string]any{"title": "long ago", "due": "2029-06-01", "assignee": asUser(w.alice.id)})
	unassigned := mk(map[string]any{"title": "nobody", "due": "2030-01-10"})
	agent := mk(map[string]any{"title": "agent", "due": "2030-01-10", "assignee": map[string]any{"kind": "agent"}})
	done := mk(map[string]any{"title": "done", "due": "2030-01-10", "assignee": asUser(w.alice.id), "column": "done"})
	onArchived := w.createBoard(w.alice, w.shared, "Later archived")
	hidden := w.createCard(w.alice, w.shared, onArchived, map[string]any{"title": "hidden", "due": "2030-01-10", "assignee": asUser(w.alice.id)})
	expect(t, w.alice.do(http.MethodPatch, boardPath(w.shared, onArchived.ID, ""),
		map[string]any{"version": onArchived.Version, "archived": true}), http.StatusOK)

	clock := &fakeClock{t: at("2030-01-02", 9)}
	sch := schedule.New(w.e.db, w.e.s.access, schedule.Options{Clock: clock.now})
	if err := w.e.s.EnableReminders(sch); err != nil {
		t.Fatal(err)
	}
	rw := &remindWorld{world: w, clock: clock, sch: sch}
	for range 2 { // and again: it is idempotent
		if err := w.e.s.ReconcileReminders(context.Background()); err != nil {
			t.Fatal(err)
		}
		wantJobs(t, rw, w.alice, future, at("2030-01-10", 8), at("2030-01-11", 8))
		// Due today, the morning is past and the overdue notice is still ahead.
		wantJobs(t, rw, w.alice, today, time.Time{}, at("2030-01-03", 8))
		for _, c := range []Card{yesterday, longAgo, unassigned, agent, done, hidden} {
			wantJobs(t, rw, w.alice, c, time.Time{}, time.Time{})
		}
	}
	var revision int64
	if err := w.e.db.QueryRow(`SELECT revision FROM scheduled_jobs WHERE dedupe_key = ? AND kind = ?`, reminderKey(future.ID), KindDueReminder).Scan(&revision); err != nil || revision != 0 {
		t.Fatalf("revision = %d, %v: a second pass must leave a job as it is", revision, err)
	}
}
