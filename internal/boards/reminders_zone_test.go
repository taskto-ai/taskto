package boards

import (
	"context"
	"net/http"
	"testing"
	"time"

	"taskto.ai/taskto/internal/notifications"
)

// The morning of a reminder is the assignee's own: 08:00 in their time zone, counted on the calendar.

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
		// 2030's clocks go forward in New York on March 10 (02:00 to 03:00) and back on November 3.
		{name: "the day before the clocks go forward", zone: "America/New_York", due: "2030-03-09", remind: utc(2030, 3, 9, 13, 0), overdue: utc(2030, 3, 10, 12, 0)},
		{name: "the day the clocks go forward", zone: "America/New_York", due: "2030-03-10", remind: utc(2030, 3, 10, 12, 0), overdue: utc(2030, 3, 11, 12, 0)},
		{name: "the day before the clocks go back", zone: "America/New_York", due: "2030-11-02", remind: utc(2030, 11, 2, 12, 0), overdue: utc(2030, 11, 3, 13, 0)},
		{name: "the day the clocks go back", zone: "America/New_York", due: "2030-11-03", remind: utc(2030, 11, 3, 13, 0), overdue: utc(2030, 11, 4, 13, 0)},
		// Lisbon's go forward on the last Sunday of March, at 01:00 UTC: not a 24-hour day.
		{name: "Lisbon's spring", zone: "Europe/Lisbon", due: "2030-03-30", remind: utc(2030, 3, 30, 8, 0), overdue: utc(2030, 3, 31, 7, 0)},
		// A southern zone's go the other way.
		{name: "Sydney's autumn", zone: "Australia/Sydney", due: "2030-04-06", remind: utc(2030, 4, 5, 21, 0), overdue: utc(2030, 4, 6, 22, 0)},
		// The last day of a month and of a year, and a leap day.
		{name: "the end of a year", zone: "Asia/Tokyo", due: "2030-12-31", remind: utc(2030, 12, 30, 23, 0), overdue: utc(2030, 12, 31, 23, 0)},
		{name: "a leap day", zone: "America/Sao_Paulo", due: "2032-02-29", remind: utc(2032, 2, 29, 11, 0), overdue: utc(2032, 3, 1, 11, 0)},
		// 08:00 on 2011-12-30 never happened in Samoa, which skipped the whole day to cross the date
		// line: the first moment after the gap, the start of the 31st (UTC+14), is the morning.
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

// setZone changes u's own time zone through the API, as the settings page does.
func (w *remindWorld) setZone(u *user, zone string) {
	w.e.t.Helper()
	expect(w.e.t, u.do(http.MethodPatch, "/api/me", map[string]any{"time_zone": zone}), http.StatusOK)
}

func TestReminderIsAtEightInTheAssigneesZone(t *testing.T) {
	w := newRemindWorld(t) // 2030-01-02, 09:00 UTC
	w.setZone(w.alice, "America/Sao_Paulo")
	c := w.newCard(map[string]any{"due": "2030-01-10", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, c, at("2030-01-10", 11), at("2030-01-11", 11))

	// Not at 08:00 UTC, which is 05:00 for her.
	w.run(at("2030-01-10", 8))
	w.run(utc(2030, 1, 10, 10, 59))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 0 {
		t.Fatalf("a reminder before 08:00 of her day: %d", n)
	}
	w.run(at("2030-01-10", 11))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("reminders at 08:00 of her day = %d, want 1", n)
	}
	// The overdue notice is at 08:00 of the next day, hers.
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
	w := newRemindWorld(t) // 2030-01-02, 09:00 UTC: 23:00 on the 2nd in Kiritimati
	w.setZone(w.alice, "Pacific/Kiritimati")
	// Due the 3rd: 08:00 there is 18:00 UTC on the 2nd, still ahead.
	c := w.newCard(map[string]any{"due": "2030-01-03", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, c, at("2030-01-02", 18), at("2030-01-03", 18))
	// Due the 2nd: 08:00 there was before now; only the overdue notice is ahead.
	d := w.newCard(map[string]any{"due": "2030-01-02", "assignee": asUser(w.alice.id)})
	wantJobs(t, w, w.alice, d, time.Time{}, at("2030-01-02", 18))
}

func TestReminderThroughTheClocksChanging(t *testing.T) {
	w := newRemindWorld(t)
	w.setZone(w.alice, "America/New_York")
	c := w.newCard(map[string]any{"due": "2030-03-09", "assignee": asUser(w.alice.id)})
	// The reminder is at 08:00 EST, the overdue notice at 08:00 EDT: an hour earlier in UTC.
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
	// No zone yet: UTC.
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))
	before, _ := w.sch.Find(context.Background(), w.e.db, w.alice.id, KindDueReminder, reminderKey(c.ID))

	w.setZone(w.alice, "Asia/Tokyo")
	wantJobs(t, w, w.alice, c, utc(2030, 1, 9, 23, 0), utc(2030, 1, 10, 23, 0))
	after, _ := w.sch.Find(context.Background(), w.e.db, w.alice.id, KindDueReminder, reminderKey(c.ID))
	if after.Occurrence() == before.Occurrence() {
		t.Fatal("a moved reminder is the same occurrence")
	}
	// The old time (08:00 UTC) passes with no word; 08:00 in Tokyo, an hour and a half ago, was the one.
	w.run(utc(2030, 1, 9, 23, 30))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("reminders = %d, want the one at 08:00 in Tokyo, which was before 08:00 UTC", n)
	}

	// A zone put back after the reminder was sent: the day's reminder is made again for 08:00 UTC,
	// and is not sent again.
	w.setZone(w.alice, "UTC")
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))
	w.run(at("2030-01-10", 9))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 1 {
		t.Fatalf("reminders after the zone went back = %d, want 1", n)
	}
	// Cleared, which is UTC too: the time is as it is, and 08:00 UTC has passed.
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

	// 08:00 in Tokyo already passed (it was 23:00 UTC the evening before): no reminder to catch up,
	// and the overdue notice is the one ahead.
	w.setZone(w.alice, "Asia/Tokyo")
	wantJobs(t, w, w.alice, mine, time.Time{}, utc(2030, 1, 10, 23, 0))
	// The owner's, in UTC, are as they were; so is a finished card, which has none.
	wantJobs(t, w, w.owner, theirs, at("2030-01-10", 8), at("2030-01-11", 8))
	wantJobs(t, w, w.alice, finished, time.Time{}, time.Time{})

	// And her change reached no one else's zone.
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

	// And someone who has no zone is on UTC.
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

	// The worker first looks at 15:30 UTC on the 10th: the 11th, 00:30 in Tokyo, where the day is
	// over (the overdue notice is the true one), and 12:30 on the 10th in São Paulo, where it isn't.
	w.run(utc(2030, 1, 10, 15, 30))
	if n := w.notices(w.alice, notifications.KindCardDue); n != 0 {
		t.Fatalf("a reminder in Tokyo after its day: %d", n)
	}
	if n := w.notices(w.owner, notifications.KindCardDue); n != 1 {
		t.Fatalf("a late reminder in São Paulo on its day = %d, want 1", n)
	}
	// São Paulo's day is still on at 02:00 UTC of the 11th (23:00 on the 10th there), where a UTC
	// date would have called it over.
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
	// Bob's change is bob's: alice's jobs, and her zone, stay.
	w.setZone(w.bob, "Asia/Tokyo")
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM users WHERE time_zone <> ''`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("users with a zone = %d, %v; want only bob", n, err)
	}
	// A bad zone changes nothing, and no one's jobs.
	expect(t, w.alice.do(http.MethodPatch, "/api/me", map[string]any{"time_zone": "Mars/Olympus"}), http.StatusBadRequest)
	wantJobs(t, w, w.alice, c, at("2030-01-10", 8), at("2030-01-11", 8))
}
