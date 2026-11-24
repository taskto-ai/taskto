package notifications_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/mail"
	"taskto.ai/taskto/internal/notifications"
)

// mailbox is the SMTP server: it keeps what the server sends, and fails as told.
type mailbox struct {
	mu   sync.Mutex
	sent []mail.Message
	fail error
}

func (m *mailbox) Send(_ context.Context, msg mail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.sent = append(m.sent, msg)
	return nil
}

func (m *mailbox) messages() []mail.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mail.Message(nil), m.sent...)
}

const publicURL = "https://taskto.example.com"

// withMail gives the service a server that can send mail, into the env's box.
func withMail(e *env, o *notifications.Options) {
	e.box = &mailbox{}
	o.Mail = mail.NewService(e.box, mail.Info{Host: "smtp.example.com"})
	o.PublicURL = publicURL
}

func newMailWorld(t *testing.T) *world {
	t.Helper()
	return worldOn(t, newEnvWith(t, true, withMail))
}

func (w *world) emailPath() string { return fmt.Sprintf("/api/workspaces/%d/notification-email", w.ws) }

func (w *world) choose(u *user, body map[string]any) notifications.EmailChoices {
	w.t.Helper()
	rec := u.do(http.MethodPut, w.emailPath(), body)
	expect(w.t, rec, http.StatusOK)
	return decode[notifications.EmailChoices](w.t, rec)
}

func (w *world) later(d time.Duration) { w.skew += d }

func TestEmailIsOffByDefaultAndNothingIsSent(t *testing.T) {
	w := newMailWorld(t)
	rec := w.bob.do(http.MethodGet, w.emailPath(), nil)
	expect(t, rec, http.StatusOK)
	got := decode[notifications.EmailChoices](t, rec)
	if !got.Available || got.Kinds[notifications.KindCardAssigned] != "off" || got.Kinds[notifications.KindAppEffectFailed] != "off" ||
		got.Digest.Hour != 8 || got.Digest.Timezone != "UTC" {
		t.Fatalf("choices = %+v", got)
	}
	w.assign("Paint the fence", w.bob)
	w.svc.DrainEmailOnce(context.Background())
	w.later(48 * time.Hour)
	w.sched.RunDue(context.Background())
	if n := len(w.box.messages()); n != 0 || w.count("email_deliveries", "1 = 1") != 0 || w.count("scheduled_jobs", "1 = 1") != 0 {
		t.Fatalf("%d mails, with email off for everyone", n)
	}
}

func TestAnInstantMailSaysOnlyWhatHappenedAndLinksBack(t *testing.T) {
	w := newMailWorld(t)
	ctx := context.Background()
	got := w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}})
	if got.Kinds[notifications.KindCardAssigned] != "instant" || got.Kinds[notifications.KindAppEffectFailed] != "off" {
		t.Fatalf("choices = %+v", got)
	}
	card := w.assign("Quarterly layoffs plan", w.bob)
	// Another kind, which bob left off, makes no mail.
	tx, _ := w.db.Begin()
	if err := notifications.Add(ctx, tx, notifications.Notice{UserID: w.bob.id, WorkspaceID: w.ws, Kind: notifications.KindAppEffectFailed, Ref: notifications.Ref{Space: w.space.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Someone else who didn't opt in is told by the inbox only.
	w.assign("Another one", w.owner)

	w.svc.DrainEmailOnce(ctx)
	msgs := w.box.messages()
	if len(msgs) != 1 || len(msgs[0].To) != 1 || msgs[0].To[0] != "bob@example.com" {
		t.Fatalf("mails = %+v", msgs)
	}
	id := w.inbox(w.bob, "").Notifications[len(w.inbox(w.bob, "").Notifications)-1].ID
	all := msgs[0].Subject + msgs[0].Text + msgs[0].HTML
	if !strings.Contains(msgs[0].Text, fmt.Sprintf("%s/notification/%d", publicURL, id)) {
		t.Fatalf("no link back in %q", msgs[0].Text)
	}
	for _, leak := range []string{"Quarterly", "layoffs", "Paint", "Team", fmt.Sprintf("%d:%d:%d", w.space.ID, w.board, card), "Owner", "Home"} {
		if strings.Contains(all, leak) {
			t.Fatalf("the mail says %q: %s", leak, all)
		}
	}
	// Sent once, even when the worker comes again.
	w.svc.DrainEmailOnce(ctx)
	if len(w.box.messages()) != 1 {
		t.Fatal("the mail was sent twice")
	}
}

func TestNoMailForWhatWasReadOrCanNoLongerBeReadOrWasTurnedOff(t *testing.T) {
	w := newMailWorld(t)
	ctx := context.Background()
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}})
	w.assign("Read already", w.bob)
	expect(t, w.bob.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/notifications/read", w.ws), nil), http.StatusNoContent)
	w.assign("Lost access", w.bob)
	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", w.ws, w.space.ID, w.bob.id), nil), http.StatusNoContent)
	w.svc.DrainEmailOnce(ctx)
	if n := len(w.box.messages()); n != 0 {
		t.Fatalf("%d mails for a read notification and one bob can't read now: %+v", n, w.box.messages())
	}

	// And turning it off before it is sent.
	expect(t, w.owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members", w.ws, w.space.ID), map[string]any{"user_id": w.bob.id, "role": access.RoleEditor}), http.StatusNoContent)
	w.assign("Turned off", w.bob)
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "off"}})
	w.svc.DrainEmailOnce(ctx)
	if n := len(w.box.messages()); n != 0 {
		t.Fatalf("%d mails after it was turned off", n)
	}
}

func TestADigestBatchesWhatHappenedAndSaysOnlyHowMany(t *testing.T) {
	w := newMailWorld(t)
	ctx := context.Background()
	// Something from before the choice isn't counted.
	w.assign("Before", w.bob)
	w.choose(w.bob, map[string]any{
		"kinds":  map[string]string{notifications.KindCardAssigned: "digest"},
		"digest": map[string]any{"hour": 8, "timezone": "Asia/Tokyo"},
	})
	if n := w.count("scheduled_jobs", "user_id = ? AND kind = 'notifications.digest'", w.bob.id); n != 1 {
		t.Fatalf("%d summaries scheduled", n)
	}
	// It is due at 8 in the morning, Tokyo's time, within a day.
	var runAt string
	if err := w.db.QueryRow(`SELECT run_at FROM scheduled_jobs WHERE user_id = ?`, w.bob.id).Scan(&runAt); err != nil {
		t.Fatal(err)
	}
	due, err := time.Parse("2006-01-02T15:04:05.000Z", runAt)
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	if err != nil || due.In(tokyo).Hour() != 8 || due.In(tokyo).Minute() != 0 || due.Before(time.Now()) || due.After(time.Now().Add(25*time.Hour)) {
		t.Fatalf("due %v (%v)", due, err)
	}

	for _, title := range []string{"Secret plan one", "Secret plan two", "Secret plan three"} {
		w.assign(title, w.bob)
	}
	w.svc.DrainEmailOnce(ctx)
	w.sched.RunDue(ctx)
	if len(w.box.messages()) != 0 {
		t.Fatal("the summary was sent before its hour")
	}
	w.later(26 * time.Hour)
	w.sched.RunDue(ctx)
	msgs := w.box.messages()
	if len(msgs) != 1 || msgs[0].To[0] != "bob@example.com" {
		t.Fatalf("mails = %+v", msgs)
	}
	all := msgs[0].Subject + msgs[0].Text + msgs[0].HTML
	if !strings.Contains(msgs[0].Text, "3 of your notifications") || !strings.Contains(msgs[0].Text, fmt.Sprintf("%s/w/%d/notifications", publicURL, w.ws)) {
		t.Fatalf("the summary = %q", msgs[0].Text)
	}
	for _, leak := range []string{"Secret", "plan", "Before", "Team"} {
		if strings.Contains(all, leak) {
			t.Fatalf("the summary says %q", leak)
		}
	}
	// What was counted isn't counted again, and with nothing new nothing is sent.
	w.later(25 * time.Hour)
	w.sched.RunDue(ctx)
	if len(w.box.messages()) != 1 {
		t.Fatalf("%d mails the day after", len(w.box.messages()))
	}
}

func TestADigestCountsNothingThePersonMayNoLongerRead(t *testing.T) {
	w := newMailWorld(t)
	ctx := context.Background()
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "digest"}})
	w.assign("Secret", w.bob)
	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", w.ws, w.space.ID, w.bob.id), nil), http.StatusNoContent)
	w.later(26 * time.Hour)
	w.sched.RunDue(ctx)
	if n := len(w.box.messages()); n != 0 {
		t.Fatalf("a summary of something bob can't read: %+v", w.box.messages())
	}
}

func TestEmailChoicesAreTheCallersOwnInTheirWorkspace(t *testing.T) {
	w := newMailWorld(t)
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "digest"}})
	// The owner and mallory see their own, not bob's.
	for _, u := range []*user{w.owner, w.mallory} {
		got := decode[notifications.EmailChoices](t, w.get(u))
		if got.Kinds[notifications.KindCardAssigned] != "off" {
			t.Fatalf("%d sees %+v", u.id, got)
		}
	}
	w.choose(w.owner, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}})
	if got := decode[notifications.EmailChoices](t, w.bob.do(http.MethodGet, w.emailPath(), nil)); got.Kinds[notifications.KindCardAssigned] != "digest" {
		t.Fatalf("bob's = %+v", got)
	}
	// A workspace the caller isn't in, and no session at all.
	for _, c := range []struct {
		u    *user
		path string
		want int
	}{
		{w.bob, fmt.Sprintf("/api/workspaces/%d/notification-email", w.ws+50), http.StatusForbidden},
		{&user{e: w.env}, w.emailPath(), http.StatusUnauthorized},
	} {
		for _, m := range []string{http.MethodGet, http.MethodPut} {
			rec := c.u.do(m, c.path, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "off"}})
			if ok := rec.Code == c.want || (c.want == http.StatusForbidden && rec.Code == http.StatusNotFound); !ok {
				t.Fatalf("%s %s = %d: %s", m, c.path, rec.Code, rec.Body)
			}
		}
	}
	if n := w.count("notification_email_prefs", "user_id = ?", w.mallory.id); n != 0 {
		t.Fatalf("mallory has %d choices", n)
	}
}

func TestEmailCannotBeTurnedOnWithoutAServerThatSendsMail(t *testing.T) {
	for name, change := range map[string]func(*env, *notifications.Options){
		"no SMTP server": func(_ *env, o *notifications.Options) { o.PublicURL = publicURL },
		"no public URL": func(e *env, o *notifications.Options) {
			e.box = &mailbox{}
			o.Mail = mail.NewService(e.box, mail.Info{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := worldOn(t, newEnvWith(t, true, change))
			got := decode[notifications.EmailChoices](t, w.bob.do(http.MethodGet, w.emailPath(), nil))
			if got.Available {
				t.Fatalf("choices = %+v", got)
			}
			rec := w.bob.do(http.MethodPut, w.emailPath(), map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}})
			expect(t, rec, http.StatusConflict)
			if !strings.Contains(rec.Body.String(), `"mail_not_configured"`) {
				t.Fatal(rec.Body.String())
			}
			// Turning things off always works, and nothing was stored or queued.
			w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "off"}})
			w.assign("x", w.bob)
			w.svc.DrainEmailOnce(context.Background())
			if w.count("notification_email_prefs", "1 = 1") != 0 || w.count("email_deliveries", "1 = 1") != 0 {
				t.Fatal("something was stored or queued")
			}
			if w.box != nil && len(w.box.messages()) != 0 {
				t.Fatal("a mail was sent")
			}
		})
	}
}

func TestEmailChoicesAreChecked(t *testing.T) {
	w := newMailWorld(t)
	for name, body := range map[string]map[string]any{
		"a kind":    {"kinds": map[string]string{"changed": "instant"}},
		"a mode":    {"kinds": map[string]string{notifications.KindCardAssigned: "always"}},
		"an hour":   {"digest": map[string]any{"hour": 24, "timezone": "UTC"}},
		"a zone":    {"digest": map[string]any{"hour": 8, "timezone": "Nowhere/Land"}},
		"local":     {"digest": map[string]any{"hour": 8, "timezone": "Local"}},
		"a field":   {"mail": "x@example.com"},
		"a long id": {"digest": map[string]any{"hour": 8, "timezone": strings.Repeat("a", 65)}},
	} {
		rec := w.bob.do(http.MethodPut, w.emailPath(), body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if w.count("notification_email_prefs", "1 = 1") != 0 {
		t.Fatal("a refused change was stored")
	}
}

func TestAFailedSendIsTriedAgainWithBoundsAndNeverShowsTheServersWords(t *testing.T) {
	w := newMailWorld(t)
	ctx := context.Background()
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}})
	w.box.fail = &mail.SendError{Stage: mail.StageConnect, Err: fmt.Errorf("dial tcp 10.1.2.3:25: refused")}
	w.assign("x", w.bob)
	w.svc.DrainEmailOnce(ctx)
	var state string
	var attempts int
	if err := w.db.QueryRow(`SELECT state, attempts FROM email_deliveries`).Scan(&state, &attempts); err != nil || state != "pending" || attempts != 1 {
		t.Fatalf("state %q after %d tries (%v)", state, attempts, err)
	}
	// Not at once again, then again after the wait, and given up after its tries.
	w.svc.DrainEmailOnce(ctx)
	for range 6 {
		w.later(2 * time.Hour)
		w.svc.DrainEmailOnce(ctx)
	}
	if err := w.db.QueryRow(`SELECT state, attempts FROM email_deliveries`).Scan(&state, &attempts); err != nil || state != "failed" || attempts != 5 {
		t.Fatalf("state %q after %d tries (%v)", state, attempts, err)
	}
	// A server that recovers is not sent the dropped one.
	w.box.fail = nil
	w.svc.DrainEmailOnce(ctx)
	if len(w.box.messages()) != 0 {
		t.Fatal("a given-up mail was sent")
	}
}

func TestLeavingTheWorkspaceEndsEmailChoicesAndTheSummary(t *testing.T) {
	w := newMailWorld(t)
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "digest"}})
	if w.count("scheduled_jobs", "user_id = ?", w.bob.id) != 1 {
		t.Fatal("no summary scheduled")
	}
	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", w.ws, w.bob.id), nil), http.StatusNoContent)
	for _, table := range []string{"notification_email_prefs", "notification_email_settings", "scheduled_jobs"} {
		if n := w.count(table, "user_id = ?", w.bob.id); n != 0 {
			t.Fatalf("%s still has %d rows for bob", table, n)
		}
	}
	// Turning the digest off ends the summary too.
	w.choose(w.mallory, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "digest"}})
	w.choose(w.mallory, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "off"}})
	if n := w.count("scheduled_jobs", "user_id = ?", w.mallory.id); n != 0 {
		t.Fatalf("%d summaries after turning it off", n)
	}
}

func (w *world) get(u *user) *httptest.ResponseRecorder {
	w.t.Helper()
	rec := u.do(http.MethodGet, w.emailPath(), nil)
	expect(w.t, rec, http.StatusOK)
	return rec
}
