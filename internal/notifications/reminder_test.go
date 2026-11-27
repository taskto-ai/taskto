package notifications_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/notifications"
)

type fakeReminders struct {
	shown map[int64]notifications.ReminderText
	asked []notifications.Ref
	users []int64
}

func (f *fakeReminders) ReminderText(_ context.Context, _ *access.Checker, userID, _ int64, ref notifications.Ref) (notifications.ReminderText, bool, error) {
	f.asked = append(f.asked, ref)
	f.users = append(f.users, userID)
	text, ok := f.shown[ref.Record]
	return text, ok, nil
}

func (w *world) remind(u *user, record int64, key string) {
	w.t.Helper()
	tx, err := w.db.Begin()
	if err != nil {
		w.t.Fatal(err)
	}
	if err := notifications.Add(context.Background(), tx, notifications.Notice{
		UserID: u.id, WorkspaceID: w.ws, Kind: notifications.KindAppReminder,
		Ref:       notifications.Ref{Space: w.space.ID, App: 9, Record: record, Reminder: "due"},
		DedupeKey: key,
	}); err != nil {
		w.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		w.t.Fatal(err)
	}
}

func TestAReminderIsRenderedFromTheRecordWhenReadAndOnlyIfTheSourceStillShowsIt(t *testing.T) {
	w := newWorld(t, false)
	src := &fakeReminders{shown: map[int64]notifications.ReminderText{7: {App: "Dues", Title: "Pay the rent"}}}
	w.svc.WithReminderSource(src)
	w.remind(w.bob, 7, "job:1")
	w.remind(w.bob, 8, "job:2")

	got := w.inbox(w.bob, "").Notifications
	if len(got) != 2 {
		t.Fatalf("inbox = %+v", got)
	}
	gone, shown := got[0], got[1]
	if gone.Kind != notifications.KindChanged || len(gone.Params) != 0 || gone.Target.Section != "home" {
		t.Fatalf("a reminder the source no longer shows = %+v", gone)
	}
	if shown.Kind != notifications.KindAppReminder || shown.Params["app"] != "Dues" || shown.Params["title"] != "Pay the rent" ||
		shown.Target.Section != "apps" || shown.Target.Item != fmt.Sprintf("%d-%d", w.space.ID, 9) {
		t.Fatalf("a shown reminder = %+v", shown)
	}
	for _, id := range src.users {
		if id != w.bob.id {
			t.Fatalf("the source was asked for user %d, not the recipient", id)
		}
	}
	for _, ref := range src.asked {
		if ref.Space != w.space.ID || ref.App != 9 || ref.Reminder != "due" {
			t.Fatalf("the source was asked for %+v", ref)
		}
	}
	if n := len(w.inbox(w.mallory, "").Notifications); n != 0 {
		t.Fatalf("mallory's inbox has %d items", n)
	}
	expect(t, w.mallory.do(http.MethodGet, fmt.Sprintf("/api/me/notifications/%d", shown.ID), nil), http.StatusNotFound)
}

func TestAReminderIsNeverShownWithNoSourceToAskAboutTheRecord(t *testing.T) {
	w := newWorld(t, false)
	w.remind(w.bob, 7, "job:1")
	got := w.inbox(w.bob, "").Notifications
	if len(got) != 1 || got[0].Kind != notifications.KindChanged || len(got[0].Params) != 0 {
		t.Fatalf("inbox = %+v", got)
	}
}

func TestAReminderPushCarriesNoRecordData(t *testing.T) {
	w := newWorld(t, true)
	src := &fakeReminders{shown: map[int64]notifications.ReminderText{7: {App: "Dues", Title: "Pay the secret rent"}}}
	w.svc.WithReminderSource(src)
	d := w.newDevice("phone")
	w.subscribe(w.bob, d)
	w.remind(w.bob, 7, "job:1")
	w.svc.DrainOnce(context.Background())

	payloads, requests := w.pushed(d)
	if len(payloads) != 1 {
		t.Fatalf("%d pushes, want 1", len(payloads))
	}
	item := w.inbox(w.bob, "").Notifications[0]
	if got := payloads[0]; len(got) != 2 || got["id"] != float64(item.ID) || got["kind"] != notifications.KindAppReminder {
		t.Fatalf("payload = %v, want only the ID and the kind", got)
	}
	for _, leak := range []string{"secret", "rent", "Dues", "record", "reminder"} {
		if bytes.Contains(requests[0].body, []byte(leak)) {
			t.Fatalf("the message is in clear on the wire: %s", leak)
		}
	}
}
