package boards

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"taskto.ai/taskto/internal/notifications"
	"taskto.ai/taskto/internal/schedule"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func utc(year int, month time.Month, day, hour, min int) time.Time {
	return time.Date(year, month, day, hour, min, 0, 0, time.UTC)
}

func TestReminderTimesAreEightOClockOfTheZone(t *testing.T) {
	for _, tc := range []struct {
		name, zone, due string
		remind, overdue time.Time
	}{
		{name: "UTC", zone: "UTC", due: "2030-01-10", remind: utc(2030, 1, 10, 8, 0), overdue: utc(2030, 1, 11, 8, 0)},
		{name: "west of UTC", zone: "America/Sao_Paulo", due: "2030-01-10", remind: utc(2030, 1, 10, 11, 0), overdue: utc(2030, 1, 11, 11, 0)},
		{name: "east of UTC", zone: "Asia/Tokyo", due: "2030-01-10", remind: utc(2030, 1, 9, 23, 0), overdue: utc(2030, 1, 10, 23, 0)},
		{name: "the furthest east", zone: "Pacific/Kiritimati", due: "2030-01-10", remind: utc(2030, 1, 9, 18, 0), overdue: utc(2030, 1, 10, 18, 0)},
		{name: "half an hour off", zone: "Asia/Kolkata", due: "2030-01-10", remind: utc(2030, 1, 10, 2, 30), overdue: utc(2030, 1, 11, 2, 30)},
		{name: "the day before the clocks go forward", zone: "America/New_York", due: "2030-03-09", remind: utc(2030, 3, 9, 13, 0), overdue: utc(2030, 3, 10, 12, 0)},
		{name: "the day the clocks go forward", zone: "America/New_York", due: "2030-03-10", remind: utc(2030, 3, 10, 12, 0), overdue: utc(2030, 3, 11, 12, 0)},
		{name: "the day before the clocks go back", zone: "America/New_York", due: "2030-11-02", remind: utc(2030, 11, 2, 12, 0), overdue: utc(2030, 11, 3, 13, 0)},
		{name: "the day the clocks go back", zone: "America/New_York", due: "2030-11-03", remind: utc(2030, 11, 3, 13, 0), overdue: utc(2030, 11, 4, 13, 0)},
		{name: "Lisbon's spring", zone: "Europe/Lisbon", due: "2030-03-30", remind: utc(2030, 3, 30, 8, 0), overdue: utc(2030, 3, 31, 7, 0)},
		{name: "Sydney's autumn", zone: "Australia/Sydney", due: "2030-04-06", remind: utc(2030, 4, 5, 21, 0), overdue: utc(2030, 4, 6, 22, 0)},
		{name: "the end of a year", zone: "Asia/Tokyo", due: "2030-12-31", remind: utc(2030, 12, 30, 23, 0), overdue: utc(2030, 12, 31, 23, 0)},
		{name: "a leap day", zone: "America/Sao_Paulo", due: "2032-02-29", remind: utc(2032, 2, 29, 11, 0), overdue: utc(2032, 3, 1, 11, 0)},
		{name: "a skipped day", zone: "Pacific/Apia", due: "2011-12-30", remind: utc(2011, 12, 30, 10, 0), overdue: utc(2011, 12, 30, 18, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remind, overdue, err := reminderTimes(tc.due, mustZone(t, tc.zone))
			if err != nil {
				t.Fatal(err)
			}
			if !remind.Equal(tc.remind) || !overdue.Equal(tc.overdue) {
				t.Fatalf("%s %s: reminder %v, overdue %v; want %v and %v", tc.zone, tc.due, remind, overdue, tc.remind, tc.overdue)
			}
		})
	}
	if _, _, err := reminderTimes("soon", time.UTC); err == nil {
		t.Fatal("a date that isn't one")
	}
}

func (w *remindWorld) setZone(u *user, zone string) {
	w.e.t.Helper()
	expect(w.e.t, u.do(http.MethodPatch, "/api/me", map[string]any{"time_zone": zone}), http.StatusOK)
}

func TestReminderIsAtEightInTheAssigneesZone(t *testing.T) {
	w := newRemindWorld(t)
	w.setZone(w.alice, "America/Sao_Paulo")
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, c, at("2030-01-10", 11), at("2030-01-11", 11))

	w.run(at("2030-01-10", 8))
	w.run(utc(2030, 1, 10, 10, 59))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 0 {
		t.Fatalf("a reminder before 08:00 of her day: %d", n)
	}
	w.run(at("2030-01-10", 11))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("reminders at 08:00 of her day = %d, want 1", n)
	}
	w.run(utc(2030, 1, 11, 10, 59))
	if n := w.notices(w.alice, notifications.KindCardOverdue); n != 0 {
		t.Fatalf("overdue before the morning after: %d", n)
	}
	w.run(at("2030-01-11", 11))
	if n := w.notices(w.alice, notifications.KindCardOverdue); n != 1 {
		t.Fatalf("overdue notices = %d, want 1", n)
	}
}

func TestReminderEastOfUTCComesTheEveningBefore(t *testing.T) {
	w := newRemindWorld(t)
	w.setZone(w.alice, "Pacific/Kiritimati")
	c := w.newCard(map[string]any{"due": "2030-01-03", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, c, at("2030-01-02", 18), at("2030-01-03", 18))
	d := w.newCard(map[string]any{"due": "2030-01-02", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, d, time.Time{}, at("2030-01-02", 18))
}

func TestReminderThroughTheClocksChanging(t *testing.T) {
	w := newRemindWorld(t)
	w.setZone(w.alice, "America/New_York")
	c := w.newCard(map[string]any{"due": "2030-03-09", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, c, utc(2030, 3, 9, 13, 0), utc(2030, 3, 10, 12, 0))
	w.run(utc(2030, 3, 10, 11, 59))
	if n := w.notices(w.alice, notifications.KindCardOverdue); n != 0 {
		t.Fatalf("overdue at 07:00 on the wall clock: %d", n)
	}
	w.run(utc(2030, 3, 10, 12, 0))
	if n := w.notices(w.alice, notifications.KindCardOverdue); n != 1 {
		t.Fatalf("overdue at 08:00 on the wall clock = %d, want 1", n)
	}
}

func TestReminderFollowsAZoneChange(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))
	before, _ := w.sch.Find(context.Background(), w.e.db, w.alice.id, KindDueReminder, reminderKey(c.ID))

	w.setZone(w.alice, "Asia/Tokyo")
	wantJobs(t, w, w.alice, c, utc(2030, 1, 9, 23, 0), utc(2030, 1, 10, 23, 0))
	after, _ := w.sch.Find(context.Background(), w.e.db, w.alice.id, KindDueReminder, reminderKey(c.ID))
	if after.Occurrence() == before.Occurrence() {
		t.Fatal("a moved reminder is the same occurrence")
	}
	w.run(utc(2030, 1, 9, 23, 30))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("reminders = %d, want the one at 08:00 in Tokyo, which was before 08:00 UTC", n)
	}

	w.setZone(w.alice, "UTC")
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))
	w.run(at("2030-01-10", 9))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("reminders after the zone went back = %d, want 1", n)
	}
	w.setZone(w.alice, "Asia/Tokyo")
	w.setZone(w.alice, "")
	wantJobs(t, w, w.alice, c, time.Time{}, at("2030-01-11", 8))
}

func TestAZoneChangeDoesNotCatchUpOrTouchOthers(t *testing.T) {
	w := newRemindWorld(t)
	mine := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	theirs := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.owner.id)})
	finished := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id), "column": "done"})
	w.run(at("2030-01-10", 7))

	w.setZone(w.alice, "Asia/Tokyo")
	wantJobs(t, w, w.alice, mine, time.Time{}, utc(2030, 1, 10, 23, 0))
	wantJobs(t, w, w.owner, theirs, at("2030-01-10", 8), at("2030-01-11", 8))
	wantJobs(t, w, w.alice, finished, time.Time{}, time.Time{})

	var zone string
	if err := w.e.db.QueryRow(`SELECT time_zone FROM users WHERE id = ?`, w.owner.id).Scan(&zone); err != nil || zone != "" {
		t.Fatalf("the owner's zone = %q, %v; want none", zone, err)
	}
}

func TestReminderAfterReassignmentIsInTheNewAssigneesZone(t *testing.T) {
	w := newRemindWorld(t)
	w.setZone(w.alice, "America/Sao_Paulo")
	w.setZone(w.owner, "Asia/Tokyo")
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, c, at("2030-01-10", 11), at("2030-01-11", 11))

	c = w.patch(c, map[string]any{"assignee": asUser(w.owner.id)})
	wantJobs(t, w, w.alice, c, time.Time{}, time.Time{})
	wantJobs(t, w, w.owner, c, utc(2030, 1, 9, 23, 0), utc(2030, 1, 10, 23, 0))

	c = w.patch(c, map[string]any{"assignee": asUser(w.bob.id)})
	wantJobs(t, w, w.bob, c, at("2030-01-10", 8), at("2030-01-11", 8))
}

func TestALateReminderIsJudgedByTheAssigneesDate(t *testing.T) {
	w := newRemindWorld(t)
	w.setZone(w.alice, "Asia/Tokyo")
	w.setZone(w.owner, "America/Sao_Paulo")
	tokyo := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	saoPaulo := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.owner.id)})
	_, _ = tokyo, saoPaulo

	w.run(utc(2030, 1, 10, 15, 30))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 0 {
		t.Fatalf("a reminder in Tokyo after its day: %d", n)
	}
	if n := w.notices(w.owner, notifications.KindCardDue); n != 1 {
		t.Fatalf("a late reminder in São Paulo on its day = %d, want 1", n)
	}
	w2 := newRemindWorld(t)
	w2.setZone(w2.owner, "America/Sao_Paulo")
	w2.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w2.owner.id)})
	w2.run(utc(2030, 1, 11, 2, 0))
	if n := w2.notices(w2.owner, notifications.KindCardDue); n != 1 {
		t.Fatalf("a reminder at 23:00 of its day = %d, want 1", n)
	}
}

func TestOnlyTheCallerSetsTheirOwnZone(t *testing.T) {
	w := newRemindWorld(t)
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	w.setZone(w.bob, "Asia/Tokyo")
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM users WHERE time_zone <> ''`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("users with a zone = %d, %v; want only bob", n, err)
	}
	expect(t, w.alice.do(http.MethodPatch, "/api/me", map[string]any{"time_zone": "Mars/Olympus"}), http.StatusBadRequest)
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))
}

func TestAZoneChangeRetimesEveryCardEvenWhereTheSchedulerIsFull(t *testing.T) {
	w := newRemindWorld(t)
	ctx := context.Background()
	fill := func(from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			_, err := w.sch.Schedule(ctx, w.e.db, schedule.Job{
				Kind: KindOverdue, UserID: w.alice.id, WorkspaceID: w.ws, SpaceID: w.shared.ID,
				DedupeKey: fmt.Sprintf("filler:%d", i), Payload: []byte(`{}`), RunAt: at("2031-01-01", 8),
			})
			if err != nil {
				t.Fatalf("filler %d: %v", i, err)
			}
		}
	}
	fill(0, schedule.MaxPerUser)
	first := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, first, time.Time{}, time.Time{})
	for _, i := range []int{0, 1} {
		if _, err := w.sch.Cancel(ctx, w.e.db, w.alice.id, KindOverdue, fmt.Sprintf("filler:%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	second := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, second, at("2030-01-10", 8), at("2030-01-11", 8))

	w.setZone(w.alice, "Asia/Tokyo")
	wantJobs(t, w, w.alice, second, utc(2030, 1, 9, 23, 0), utc(2030, 1, 10, 23, 0))
	wantJobs(t, w, w.alice, first, time.Time{}, time.Time{})
}

func TestReminderOfASkippedDayIsDeliveredWhenTheClocksJump(t *testing.T) {
	w := newRemindWorld(t)
	w.clock.set(utc(2011, 12, 1, 0, 0))
	w.setZone(w.alice, "Pacific/Apia")
	c := w.newCard(map[string]any{"due": "2011-12-30", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, c, utc(2011, 12, 30, 10, 0), utc(2011, 12, 30, 18, 0))
	w.run(utc(2011, 12, 30, 9, 59))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 0 {
		t.Fatalf("a reminder before the clocks jumped: %d", n)
	}
	w.run(utc(2011, 12, 30, 10, 0))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("reminders at the start of the date the clocks jumped to = %d, want 1", n)
	}
}
