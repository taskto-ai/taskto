package notifications_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	// verify are the mails that confirm an address, kept apart from the notifications.
	verify []mail.Message
	fail   error
	// hold, when set, keeps a send waiting until it is closed (or the send's context ends).
	hold chan struct{}
	// pause is how long each send takes.
	pause time.Duration
	// onSend runs as a send begins, before it answers.
	onSend func()
	// entered counts the sends begun.
	entered int
	// last is the newest message handed to the relay, whether it took it or not.
	last mail.Message
}

func (m *mailbox) Send(ctx context.Context, msg mail.Message) error {
	m.mu.Lock()
	hold, pause := m.hold, m.pause
	m.entered++
	onSend := m.onSend
	m.mu.Unlock()
	if onSend != nil {
		onSend()
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return &mail.SendError{Stage: mail.StageSend, Err: ctx.Err()}
		}
	}
	time.Sleep(pause)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = msg
	if m.fail != nil {
		return m.fail
	}
	if strings.HasPrefix(msg.Subject, "Confirm your email address") {
		m.verify = append(m.verify, msg)
		return nil
	}
	m.sent = append(m.sent, msg)
	return nil
}

func (m *mailbox) confirmations() []mail.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mail.Message(nil), m.verify...)
}

// lastToken is the token in the newest confirmation mail's link.
func (m *mailbox) lastToken(t *testing.T) string {
	t.Helper()
	all := m.confirmations()
	if len(all) == 0 {
		t.Fatal("no confirmation mail")
	}
	_, rest, ok := strings.Cut(all[len(all)-1].Text, publicURL+"/confirm-notification-email#")
	if !ok {
		t.Fatalf("no link in %q", all[len(all)-1].Text)
	}
	return strings.Fields(rest)[0]
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

// put changes u's choices, as they are, and answers the new ones.
func (w *world) put(u *user, body map[string]any) notifications.EmailChoices {
	w.t.Helper()
	rec := u.do(http.MethodPut, w.emailPath(), body)
	expect(w.t, rec, http.StatusOK)
	return decode[notifications.EmailChoices](w.t, rec)
}

// confirm opens the link of the newest confirmation mail as u.
func (w *world) confirm(u *user) {
	w.t.Helper()
	expect(w.t, u.do(http.MethodPost, "/api/me/notification-email/confirm", map[string]string{"token": w.box.lastToken(w.t)}), http.StatusNoContent)
}

// choose changes u's choices and, as a person who opens the link they are mailed would, confirms
// their address if it isn't yet.
func (w *world) choose(u *user, body map[string]any) notifications.EmailChoices {
	w.t.Helper()
	got := w.put(u, body)
	if got.Address.State == notifications.AddressVerified {
		return got
	}
	w.confirm(u)
	return w.put(u, map[string]any{})
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
			w.put(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "off"}})
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

// ---- At most once ----

// ledger puts a row in the ledger as the server leaves it when it stops after it wrote the attempt.
func (w *world) ledger(userID int64, key, kind, state string, upto int64) {
	w.t.Helper()
	now := time.Now().Add(w.skew).UTC().Format("2006-01-02T15:04:05.000Z")
	if _, err := w.db.Exec(`INSERT INTO notification_email_ledger (user_id, key, kind, state, upto, attempts, reserved_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?)`, userID, key, kind, state, upto, now, now); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) maxNotification() (id int64) {
	w.t.Helper()
	if err := w.db.QueryRow(`SELECT coalesce(max(id), 0) FROM notifications`).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

func TestAnAttemptThatStoppedAfterTheRelayMayHaveTakenItIsNotSentAgain(t *testing.T) {
	w := newMailWorld(t)
	ctx := context.Background()
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant", notifications.KindAppEffectFailed: "digest"}})

	// The server was stopped after the attempt was written and before its outcome was.
	w.assign("Instant", w.bob)
	w.ledger(w.bob.id, fmt.Sprintf("instant:%d", w.maxNotification()), "instant", "sending", 0)
	w.svc.DrainEmailOnce(ctx)
	if n := len(w.box.messages()); n != 0 {
		t.Fatalf("%d mails for an instant mail that may have been sent already", n)
	}
	if n := w.count("email_deliveries", "state = 'pending'"); n != 0 {
		t.Fatalf("%d deliveries still wait to be tried again", n)
	}

	// The same for the summary: its occurrence is what the key names, and what it covered is
	// not counted again.
	addEffectFailure(t, w, w.bob)
	upto := w.maxNotification()
	job, err := w.sched.Find(ctx, w.db, w.bob.id, notifications.KindDigest, fmt.Sprintf("workspace:%d", w.ws))
	if err != nil {
		t.Fatal(err)
	}
	w.ledger(w.bob.id, "digest:"+job.Occurrence(), "digest", "sending", upto)
	w.later(26 * time.Hour)
	w.sched.RunDue(ctx)
	if n := len(w.box.messages()); n != 0 {
		t.Fatalf("%d mails for a summary that may have been sent already: %+v", n, w.box.messages())
	}
	var cursor int64
	if err := w.db.QueryRow(`SELECT digest_cursor FROM notification_email_settings WHERE user_id = ?`, w.bob.id).Scan(&cursor); err != nil || cursor != upto {
		t.Fatalf("cursor %d, want %d (%v)", cursor, upto, err)
	}
	w.later(25 * time.Hour)
	w.sched.RunDue(ctx)
	if n := len(w.box.messages()); n != 0 {
		t.Fatalf("%d mails the day after", n)
	}
}

// addEffectFailure makes an unread notification of the kind of an app action that failed.
func addEffectFailure(t *testing.T, w *world, u *user) {
	t.Helper()
	var app int64
	if err := w.db.QueryRow(`INSERT INTO apps (space_id, name, definition) VALUES (?, ?, '{}') RETURNING id`,
		w.space.ID, fmt.Sprintf("App %d", time.Now().UnixNano())).Scan(&app); err != nil {
		t.Fatal(err)
	}
	tx, err := w.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := notifications.Add(context.Background(), tx, notifications.Notice{UserID: u.id, WorkspaceID: w.ws, Kind: notifications.KindAppEffectFailed, Ref: notifications.Ref{Space: w.space.ID, App: app}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	w.stampNow()
}

// stampNow dates the newest notification at the service's clock, which a test moves on: Add dates
// them at the real time.
func (w *world) stampNow() {
	w.t.Helper()
	if _, err := w.db.Exec(`UPDATE notifications SET created_at = ? WHERE id = (SELECT max(id) FROM notifications)`,
		time.Now().Add(w.skew).UTC().Format("2006-01-02T15:04:05.000Z")); err != nil {
		w.t.Fatal(err)
	}
}

func TestALostAnswerToTheMessageIsNeverRetriedButARefusalIs(t *testing.T) {
	w := newMailWorld(t)
	ctx := context.Background()
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}})

	// The connection was lost after the message was sent: the relay may have it.
	w.box.fail = &mail.SendError{Stage: mail.StageRejected, Ambiguous: true, Err: fmt.Errorf("connection reset")}
	w.assign("Lost answer", w.bob)
	w.svc.DrainEmailOnce(ctx)
	w.box.fail = nil
	for range 6 {
		w.later(2 * time.Hour)
		w.svc.DrainEmailOnce(ctx)
	}
	if n := len(w.box.messages()); n != 0 {
		t.Fatalf("an ambiguous failure was tried again: %d mails", n)
	}
	var state string
	if err := w.db.QueryRow(`SELECT state FROM notification_email_ledger WHERE kind = 'instant'`).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("ledger state %q (%v)", state, err)
	}

	// A refusal before the message was taken is tried again, and sent once.
	w.box.fail = &mail.SendError{Stage: mail.StageRejected, Code: 451, Err: fmt.Errorf("try later")}
	w.assign("Refused first", w.bob)
	w.svc.DrainEmailOnce(ctx)
	w.box.fail = nil
	w.later(2 * time.Hour)
	w.svc.DrainEmailOnce(ctx)
	w.svc.DrainEmailOnce(ctx)
	if n := len(w.box.messages()); n != 1 {
		t.Fatalf("%d mails after a refusal and a retry", n)
	}

	// The same for a summary: a lost answer is not retried (nor repeated the next time).
	w2 := newMailWorld(t)
	w2.choose(w2.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "digest"}})
	w2.assign("One", w2.bob)
	w2.box.fail = &mail.SendError{Stage: mail.StageRejected, Ambiguous: true, Err: fmt.Errorf("connection reset")}
	w2.later(26 * time.Hour)
	w2.sched.RunDue(ctx)
	w2.box.fail = nil
	w2.later(2 * time.Hour)
	w2.sched.RunDue(ctx)
	w2.later(25 * time.Hour)
	w2.sched.RunDue(ctx)
	if n := len(w2.box.messages()); n != 0 {
		t.Fatalf("%d summaries after a lost answer", n)
	}
	if w2.count("scheduled_jobs", "state = 'pending' AND user_id = ?", w2.bob.id) != 1 {
		t.Fatal("the next summary isn't scheduled")
	}
}

// ---- A slow SMTP server ----

func TestASlowMailServerDoesNotDelayPush(t *testing.T) {
	w := newMailWorld(t)
	ctx := context.Background()
	d := w.newDevice("bob-phone")
	w.subscribe(w.bob, d)
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}})
	hold := make(chan struct{})
	w.box.mu.Lock()
	w.box.hold = hold
	base := w.box.entered // the confirmation mail was a send too
	w.box.mu.Unlock()
	w.assign("First", w.bob)

	stop := w.svc.Start(ctx)
	defer stop()
	deadline := time.Now().Add(10 * time.Second)
	wait := func(what string, done func() bool) {
		t.Helper()
		for !done() {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	// The mail server holds the first mail; a notification made meanwhile is still pushed.
	wait("the mail server was never asked", func() bool {
		w.box.mu.Lock()
		defer w.box.mu.Unlock()
		return w.box.entered > base
	})
	w.assign("Second", w.bob)
	wait("the push waited for the mail server", func() bool { p, _ := w.pushed(d); return len(p) == 2 })
	// And one more, while the first mail is still held and the second waits behind it.
	w.assign("Third", w.bob)
	wait("a later push waited for the mail server", func() bool { p, _ := w.pushed(d); return len(p) == 3 })
	if n := len(w.box.messages()); n != 0 {
		t.Fatalf("the mail was sent while the server held it: %d", n)
	}
	close(hold)
	wait("the mails were never sent", func() bool { return len(w.box.messages()) == 3 })
}

// ---- The hourly limit ----

func TestTheHourlyLimitHoldsForConcurrentWorkersAndForgetsNothing(t *testing.T) {
	w := newMailWorld(t)
	ctx := context.Background()
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}})
	w.box.mu.Lock()
	w.box.pause = 2 * time.Millisecond
	w.box.mu.Unlock()
	for i := range 25 {
		w.assign(fmt.Sprintf("Card %d", i), w.bob)
	}
	var running sync.WaitGroup
	for range 4 {
		running.Add(1)
		go func() {
			defer running.Done()
			w.svc.DrainEmailOnce(ctx)
		}()
	}
	running.Wait()
	if n := len(w.box.messages()); n != notifications.MaxInstantPerHour {
		t.Fatalf("%d mails in the hour, the most is %d", n, notifications.MaxInstantPerHour)
	}
	// Deleting the notifications (retention, or leaving a workspace) doesn't free slots.
	if _, err := w.db.Exec(`DELETE FROM notifications WHERE user_id = ?`, w.bob.id); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		w.assign(fmt.Sprintf("Later %d", i), w.bob)
	}
	w.svc.DrainEmailOnce(ctx)
	if n := len(w.box.messages()); n != notifications.MaxInstantPerHour {
		t.Fatalf("%d mails after the history was deleted", n)
	}
	// An hour later the slots are free again.
	w.later(61 * time.Minute)
	w.assign("Next hour", w.bob)
	w.svc.DrainEmailOnce(ctx)
	if n := len(w.box.messages()); n != notifications.MaxInstantPerHour+1 {
		t.Fatalf("%d mails an hour later", n)
	}
}

// ---- The last point that can cancel ----

func TestWhatChangesJustBeforeTheSubmissionCancelsIt(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(w *world){
		"opting out": func(w *world) {
			w.put(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "off"}})
		},
		"losing access": func(w *world) {
			expect(w.t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", w.ws, w.space.ID, w.bob.id), nil), http.StatusNoContent)
		},
		"reading it": func(w *world) {
			expect(w.t, w.bob.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/notifications/read", w.ws), nil), http.StatusNoContent)
		},
	} {
		t.Run("instant, "+name, func(t *testing.T) {
			w := newMailWorld(t)
			w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}})
			w.assign("Racing", w.bob)
			w.svc.AfterReserve(func() { change(w) })
			w.svc.DrainEmailOnce(ctx)
			if n := len(w.box.messages()); n != 0 {
				t.Fatalf("%d mails after %s while it was being sent", n, name)
			}
			if n := w.count("notification_email_ledger", "state = 'cancelled'"); n != 1 {
				t.Fatalf("%d cancelled attempts", n)
			}
		})
	}
	for name, change := range map[string]func(w *world){
		"opting out": func(w *world) {
			w.put(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "off"}})
		},
		"a changed hour": func(w *world) {
			w.put(w.bob, map[string]any{"digest": map[string]any{"hour": 9, "timezone": "UTC"}})
		},
		"losing access": func(w *world) {
			expect(w.t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/spaces/%d/members/%d", w.ws, w.space.ID, w.bob.id), nil), http.StatusNoContent)
		},
	} {
		t.Run("summary, "+name, func(t *testing.T) {
			w := newMailWorld(t)
			w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "digest"}})
			w.assign("Racing", w.bob)
			w.svc.AfterReserve(func() { change(w) })
			w.later(26 * time.Hour)
			w.sched.RunDue(ctx)
			if n := len(w.box.messages()); n != 0 {
				t.Fatalf("%d summaries after %s while it was being sent", n, name)
			}
			if n := w.count("notification_email_ledger", "kind = 'digest' AND state = 'cancelled'"); n != 1 {
				t.Fatalf("%d cancelled attempts", n)
			}
		})
	}
}

// ---- Daylight saving time ----

func TestTheSummaryStaysAtTheLocalHourWhenTheClocksChange(t *testing.T) {
	w := newMailWorld(t)
	ctx := context.Background()
	// The evening before the clocks go back in Lisbon (the last Sunday of October, 2027): 8 o'clock
	// is 07:00 UTC on the 30th, and 08:00 UTC on the 31st. A fixed 24 hours would say 07:00 again.
	w.skew = time.Until(time.Date(2027, 10, 30, 6, 0, 0, 0, time.UTC))
	w.choose(w.bob, map[string]any{
		"kinds":  map[string]string{notifications.KindCardAssigned: "digest"},
		"digest": map[string]any{"hour": 8, "timezone": "Europe/Lisbon"},
	})
	runAt := func() string {
		var at string
		if err := w.db.QueryRow(`SELECT run_at FROM scheduled_jobs WHERE user_id = ?`, w.bob.id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if got := runAt(); got != "2027-10-30T07:00:00.000Z" {
		t.Fatalf("first summary at %s", got)
	}
	w.skew = time.Until(time.Date(2027, 10, 30, 7, 1, 0, 0, time.UTC))
	w.sched.RunDue(ctx)
	if got := runAt(); got != "2027-10-31T08:00:00.000Z" {
		t.Fatalf("the summary after the clocks went back is at %s, want 08:00 local (08:00 UTC)", got)
	}
	// And forward again in spring: 08:00 local is 07:00 UTC in summer (the 28th of March 2027 is
	// the day they go forward; the 27th is 08:00 UTC, the 28th 07:00 UTC).
	w2 := newMailWorld(t)
	w2.skew = time.Until(time.Date(2027, 3, 27, 7, 0, 0, 0, time.UTC))
	w2.choose(w2.bob, map[string]any{
		"kinds":  map[string]string{notifications.KindCardAssigned: "digest"},
		"digest": map[string]any{"hour": 8, "timezone": "Europe/Lisbon"},
	})
	w2.skew = time.Until(time.Date(2027, 3, 27, 8, 1, 0, 0, time.UTC))
	w2.sched.RunDue(ctx)
	var at string
	if err := w2.db.QueryRow(`SELECT run_at FROM scheduled_jobs WHERE user_id = ?`, w2.bob.id).Scan(&at); err != nil || at != "2027-03-28T07:00:00.000Z" {
		t.Fatalf("the summary after the clocks went forward is at %s (%v)", at, err)
	}
}

// ---- The summary's start ----

func TestASummaryOfAKindStartsWhereTheChoiceWasMade(t *testing.T) {
	w := newMailWorld(t)
	ctx := context.Background()
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "digest"}})
	// A notification of a kind that is not in the summary yet, unread and old by the time it is.
	addEffectFailure(t, w, w.bob)
	w.later(time.Hour)
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindAppEffectFailed: "digest"}})
	w.later(26 * time.Hour)
	w.sched.RunDue(ctx)
	if n := len(w.box.messages()); n != 0 {
		t.Fatalf("a summary of what came before the choice: %+v", w.box.messages())
	}
	// What mailed as it happened isn't counted either when the kind moves to the summary.
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindAppEffectFailed: "instant"}})
	addEffectFailure(t, w, w.bob)
	w.svc.DrainEmailOnce(ctx)
	if n := len(w.box.messages()); n != 1 {
		t.Fatalf("%d instant mails", n)
	}
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindAppEffectFailed: "digest"}})
	w.later(25 * time.Hour)
	w.sched.RunDue(ctx)
	if n := len(w.box.messages()); n != 1 {
		t.Fatalf("what was mailed already was counted in a summary: %d mails", n)
	}
	// But what comes after is.
	addEffectFailure(t, w, w.bob)
	w.later(25 * time.Hour)
	w.sched.RunDue(ctx)
	if n := len(w.box.messages()); n != 2 || !strings.Contains(w.box.messages()[1].Text, "1 of your notifications") {
		t.Fatalf("mails = %+v", w.box.messages())
	}
}

// ---- Confirming the address ----

func TestNothingIsMailedBeforeTheAddressIsConfirmed(t *testing.T) {
	w := newMailWorld(t)
	ctx := context.Background()
	got := w.put(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant", notifications.KindAppEffectFailed: "digest"}})
	if got.Address.Email != "bob@example.com" || got.Address.State != notifications.AddressPending {
		t.Fatalf("address = %+v", got.Address)
	}
	links := w.box.confirmations()
	if len(links) != 1 || links[0].To[0] != "bob@example.com" || !strings.Contains(links[0].Text, publicURL+"/confirm-notification-email#") {
		t.Fatalf("confirmation mails = %+v", links)
	}
	// Turning something else on doesn't send another link while one waits.
	w.put(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "digest"}})
	if n := len(w.box.confirmations()); n != 1 {
		t.Fatalf("%d links", n)
	}
	w.put(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}})

	w.assign("Before", w.bob)
	addEffectFailure(t, w, w.bob)
	w.svc.DrainEmailOnce(ctx)
	w.later(26 * time.Hour)
	w.sched.RunDue(ctx)
	if n := len(w.box.messages()); n != 0 || w.count("email_deliveries", "1 = 1") != 0 {
		t.Fatalf("%d mails to an address nobody confirmed", n)
	}

	// A link that waited past its day is replaced by asking for another.
	expect(t, w.bob.do(http.MethodPost, w.emailPath()+"/verification", nil), http.StatusNoContent)
	token := w.box.lastToken(t)

	// The token is stored only as its hash.
	sum := sha256.Sum256([]byte(token))
	if n := w.count("notification_email_verification", "token_hash = ?", hex.EncodeToString(sum[:])); n != 1 {
		t.Fatal("the token isn't stored as its SHA-256")
	}
	if n := w.count("notification_email_verification", "token_hash = ? OR address = ?", token, token); n != 0 {
		t.Fatal("the token is stored as it is")
	}

	// Someone else, signed in, can't use it; it stays good for its owner, once.
	rec := w.mallory.do(http.MethodPost, "/api/me/notification-email/confirm", map[string]string{"token": token})
	expect(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), `"notification_email_verification_invalid"`) {
		t.Fatal(rec.Body.String())
	}
	for _, bad := range []string{"", "nonsense", token + "x"} {
		expect(t, w.bob.do(http.MethodPost, "/api/me/notification-email/confirm", map[string]string{"token": bad}), http.StatusBadRequest)
	}
	expect(t, (&user{e: w.env}).do(http.MethodPost, "/api/me/notification-email/confirm", map[string]string{"token": token}), http.StatusUnauthorized)
	expect(t, w.bob.do(http.MethodPost, "/api/me/notification-email/confirm", map[string]string{"token": token}), http.StatusNoContent)
	expect(t, w.bob.do(http.MethodPost, "/api/me/notification-email/confirm", map[string]string{"token": token}), http.StatusBadRequest)
	if got := decode[notifications.EmailChoices](t, w.get(w.bob)); got.Address.State != notifications.AddressVerified {
		t.Fatalf("address = %+v", got.Address)
	}

	// From then on mail goes out.
	w.assign("After", w.bob)
	w.stampNow()
	w.svc.DrainEmailOnce(ctx)
	if n := len(w.box.messages()); n != 1 {
		t.Fatalf("%d mails after the address was confirmed", n)
	}
}

func TestAConfirmationLinkExpiresIsReplacedAndIsLimited(t *testing.T) {
	w := newMailWorld(t)
	w.put(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}})
	first := w.box.lastToken(t)

	// Asking again at once is refused, with the wait.
	rec := w.bob.do(http.MethodPost, w.emailPath()+"/verification", nil)
	expect(t, rec, http.StatusTooManyRequests)
	if !strings.Contains(rec.Body.String(), `"notification_email_verification_too_soon"`) || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, rec.Header())
	}
	if n := len(w.box.confirmations()); n != 1 {
		t.Fatalf("%d links", n)
	}
	// Later a new one is sent, and the old link no longer works.
	w.later(6 * time.Minute)
	expect(t, w.bob.do(http.MethodPost, w.emailPath()+"/verification", nil), http.StatusNoContent)
	if n := len(w.box.confirmations()); n != 2 {
		t.Fatalf("%d links", n)
	}
	expect(t, w.bob.do(http.MethodPost, "/api/me/notification-email/confirm", map[string]string{"token": first}), http.StatusBadRequest)

	// A link expires.
	second := w.box.lastToken(t)
	w.later(25 * time.Hour)
	expect(t, w.bob.do(http.MethodPost, "/api/me/notification-email/confirm", map[string]string{"token": second}), http.StatusBadRequest)
	if got := decode[notifications.EmailChoices](t, w.get(w.bob)); got.Address.State != notifications.AddressUnverified {
		t.Fatalf("address = %+v", got.Address)
	}
	expect(t, w.bob.do(http.MethodPost, w.emailPath()+"/verification", nil), http.StatusNoContent)
	w.confirm(w.bob)

	// A confirmed address is sent nothing more, however often it is asked.
	expect(t, w.bob.do(http.MethodPost, w.emailPath()+"/verification", nil), http.StatusNoContent)
	if n := len(w.box.confirmations()); n != 3 {
		t.Fatalf("%d links", n)
	}
}

func TestAChangedAddressIsConfirmedAgain(t *testing.T) {
	w := newMailWorld(t)
	ctx := context.Background()
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}})
	if _, err := w.db.Exec(`UPDATE users SET email = 'typo@example.com' WHERE id = ?`, w.bob.id); err != nil {
		t.Fatal(err)
	}
	if got := decode[notifications.EmailChoices](t, w.get(w.bob)); got.Address.State != notifications.AddressUnverified || got.Address.Email != "typo@example.com" {
		t.Fatalf("address = %+v", got.Address)
	}
	w.assign("To a typo", w.bob)
	w.svc.DrainEmailOnce(ctx)
	if n := len(w.box.messages()); n != 0 {
		t.Fatalf("%d mails to a changed address", n)
	}
	// The next choice sends the link, to the new address.
	w.later(6 * time.Minute)
	w.put(w.bob, map[string]any{"kinds": map[string]string{notifications.KindAppEffectFailed: "instant"}})
	links := w.box.confirmations()
	if last := links[len(links)-1]; len(links) != 2 || last.To[0] != "typo@example.com" {
		t.Fatalf("links = %+v", links)
	}
}

func TestAConfirmationThatCouldNotBeSentLeavesNoLinkAndLogsNoToken(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	w := newMailWorld(t)
	w.box.fail = &mail.SendError{Stage: mail.StageConnect, Err: fmt.Errorf("dial tcp 10.1.2.3:25: refused")}
	// The choice is kept; the link isn't sent, and the settings say so.
	got := w.put(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}})
	if got.Kinds[notifications.KindCardAssigned] != "instant" || got.Address.State != notifications.AddressUnverified {
		t.Fatalf("choices = %+v", got)
	}
	w.box.mu.Lock()
	tried := w.box.last
	w.box.mu.Unlock()
	if _, token, ok := strings.Cut(tried.Text, "/confirm-notification-email#"); !ok || strings.Contains(logs.String(), strings.Fields(token)[0]) {
		t.Fatalf("no link was tried, or a log has its token: %q", logs.String())
	}
	rec := w.bob.do(http.MethodPost, w.emailPath()+"/verification", nil)
	expect(t, rec, http.StatusBadGateway)
	if !strings.Contains(rec.Body.String(), `"notification_email_verification_unavailable"`) || strings.Contains(rec.Body.String(), "10.1.2.3") {
		t.Fatal(rec.Body.String())
	}
	// A failure doesn't use up the wait: the next try is allowed at once.
	w.box.fail = nil
	expect(t, w.bob.do(http.MethodPost, w.emailPath()+"/verification", nil), http.StatusNoContent)
	if n := len(w.box.confirmations()); n != 1 {
		t.Fatalf("%d links", n)
	}
}

func TestOnlyMembersAskForAConfirmationOfTheirOwnAddress(t *testing.T) {
	w := newMailWorld(t)
	for _, c := range []struct {
		u    *user
		path string
		want int
	}{
		{w.bob, fmt.Sprintf("/api/workspaces/%d/notification-email/verification", w.ws+50), http.StatusForbidden},
		{&user{e: w.env}, w.emailPath() + "/verification", http.StatusUnauthorized},
	} {
		rec := c.u.do(http.MethodPost, c.path, nil)
		if ok := rec.Code == c.want || (c.want == http.StatusForbidden && rec.Code == http.StatusNotFound); !ok {
			t.Fatalf("%s = %d: %s", c.path, rec.Code, rec.Body)
		}
	}
	if n := len(w.box.confirmations()); n != 0 {
		t.Fatalf("%d links sent", n)
	}
	// Nobody else's address gets the link: mallory asks, and it goes to mallory.
	expect(t, w.mallory.do(http.MethodPost, w.emailPath()+"/verification", nil), http.StatusNoContent)
	if links := w.box.confirmations(); len(links) != 1 || links[0].To[0] != "mallory@example.com" {
		t.Fatalf("links = %+v", links)
	}
}

// ---- Turning it off ----

func TestWhereMailCannotBeSentTheChoicesCanStillBeTurnedOff(t *testing.T) {
	w := newMailWorld(t)
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}})
	// The same data on a server that lost its mail: reading it says so, and off still works.
	w.svc.SetMail(nil)
	got := decode[notifications.EmailChoices](t, w.get(w.bob))
	if got.Available || got.Kinds[notifications.KindCardAssigned] != "instant" {
		t.Fatalf("choices = %+v", got)
	}
	got = w.put(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "off"}})
	if got.Kinds[notifications.KindCardAssigned] != "off" {
		t.Fatalf("choices = %+v", got)
	}
}

// ---- Review fixes ----

func TestSavingAnotherChoiceLeavesADueSummaryWhereItWas(t *testing.T) {
	w := newMailWorld(t)
	ctx := context.Background()
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "digest"}})
	w.assign("Today", w.bob)
	// It is due, and another kind is chosen (and an empty save) before the worker comes to it.
	w.later(26 * time.Hour)
	var before int64
	if err := w.db.QueryRow(`SELECT revision FROM scheduled_jobs WHERE user_id = ?`, w.bob.id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	w.put(w.bob, map[string]any{"kinds": map[string]string{notifications.KindAppEffectFailed: "instant"}})
	w.put(w.bob, map[string]any{})
	var after int64
	if err := w.db.QueryRow(`SELECT revision FROM scheduled_jobs WHERE user_id = ?`, w.bob.id).Scan(&after); err != nil || after != before {
		t.Fatalf("revision %d, was %d (%v)", after, before, err)
	}
	w.sched.RunDue(ctx)
	if n := len(w.box.messages()); n != 1 {
		t.Fatalf("%d summaries", n)
	}
	// A change of the hour does move it, to the next 09:00 UTC.
	if err := w.db.QueryRow(`SELECT revision FROM scheduled_jobs WHERE user_id = ?`, w.bob.id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	w.put(w.bob, map[string]any{"digest": map[string]any{"hour": 9, "timezone": "UTC"}})
	var at string
	if err := w.db.QueryRow(`SELECT revision, run_at FROM scheduled_jobs WHERE user_id = ?`, w.bob.id).Scan(&after, &at); err != nil || after == before {
		t.Fatalf("revision %d after a new hour, was %d (%v)", after, before, err)
	}
	if due, err := time.Parse("2006-01-02T15:04:05.000Z", at); err != nil || due.Hour() != 9 || !due.After(time.Now().Add(w.skew)) {
		t.Fatalf("run_at %s (%v), want the next 09:00 UTC", at, err)
	}
}

func TestAnHourThatTheClocksSkipIsTheFirstMomentAfterTheGap(t *testing.T) {
	w := newMailWorld(t)
	// The US clocks skip 02:00 on 14 March 2027 (02:00 EST is 03:00 EDT).
	w.skew = time.Until(time.Date(2027, 3, 13, 12, 0, 0, 0, time.UTC))
	w.choose(w.bob, map[string]any{
		"kinds":  map[string]string{notifications.KindCardAssigned: "digest"},
		"digest": map[string]any{"hour": 2, "timezone": "America/New_York"},
	})
	var at string
	if err := w.db.QueryRow(`SELECT run_at FROM scheduled_jobs WHERE user_id = ?`, w.bob.id).Scan(&at); err != nil || at != "2027-03-14T07:00:00.000Z" {
		t.Fatalf("run_at %s (%v), want 03:00 EDT", at, err)
	}
	// An ordinary hour keeps its wall-clock hour across the change: today's 08:00 is EST, tomorrow's EDT.
	w.skew = time.Until(time.Date(2027, 3, 13, 14, 0, 0, 0, time.UTC))
	w.put(w.bob, map[string]any{"digest": map[string]any{"hour": 8, "timezone": "America/New_York"}})
	if err := w.db.QueryRow(`SELECT run_at FROM scheduled_jobs WHERE user_id = ?`, w.bob.id).Scan(&at); err != nil || at != "2027-03-14T12:00:00.000Z" {
		t.Fatalf("run_at %s (%v), want 08:00 EDT", at, err)
	}
}

func TestAnExpiredHandlerStillSchedulesTheNextSummary(t *testing.T) {
	w := newMailWorld(t)
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "digest"}})
	job, err := w.sched.Find(context.Background(), w.db, w.bob.id, notifications.KindDigest, fmt.Sprintf("workspace:%d", w.ws))
	if err != nil {
		t.Fatal(err)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.svc.DigestNext(ended, job); err != nil {
		t.Fatal(err)
	}
	next, err := w.sched.Find(context.Background(), w.db, w.bob.id, notifications.KindDigest, fmt.Sprintf("workspace:%d", w.ws))
	if err != nil || next.Revision != job.Revision+1 {
		t.Fatalf("revision %d, was %d (%v)", next.Revision, job.Revision, err)
	}
}

func TestASummaryRecordsWhatItCoversBeforeItIsSubmitted(t *testing.T) {
	w := newMailWorld(t)
	w.choose(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "digest"}})
	w.assign("One", w.bob)
	// One more arrives between the plan that reserved the attempt and the last check.
	w.svc.AfterReserve(func() { w.assign("Two", w.bob) })
	var upto, covered int64
	w.box.mu.Lock()
	w.box.onSend = func() {
		_ = w.db.QueryRow(`SELECT upto FROM notification_email_ledger WHERE kind = 'digest'`).Scan(&upto)
		covered = w.maxNotification()
	}
	w.box.mu.Unlock()
	w.later(26 * time.Hour)
	w.sched.RunDue(context.Background())
	if len(w.box.messages()) != 1 || upto == 0 || upto != covered {
		t.Fatalf("the ledger said %d at the send, the message covers up to %d", upto, covered)
	}
}

func TestAnAmbiguousConfirmationSendKeepsTheLink(t *testing.T) {
	w := newMailWorld(t)
	w.box.fail = &mail.SendError{Stage: mail.StageRejected, Ambiguous: true, Err: fmt.Errorf("connection reset")}
	// The relay took it and lost its answer: the link it holds must work, and the wait stands.
	got := w.put(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}})
	if got.Address.State != notifications.AddressPending {
		t.Fatalf("address = %+v", got.Address)
	}
	w.box.mu.Lock()
	tried := w.box.last
	w.box.mu.Unlock()
	_, rest, ok := strings.Cut(tried.Text, "/confirm-notification-email#")
	if !ok {
		t.Fatal("no link was tried")
	}
	expect(t, w.bob.do(http.MethodPost, "/api/me/notification-email/confirm", map[string]string{"token": strings.Fields(rest)[0]}), http.StatusNoContent)
}

func TestAChoiceIsNotStoredForSomeoneWhoLeftMeanwhileOrFromNull(t *testing.T) {
	w := newMailWorld(t)
	expect(t, w.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", w.ws, w.bob.id), nil), http.StatusNoContent)
	// The route's check passed before the removal; the write is the last word.
	err := w.svc.SetEmailChoices(context.Background(), w.bob.id, w.ws, notifications.EmailUpdate{Kinds: map[string]string{notifications.KindCardAssigned: "instant"}})
	if !errors.Is(err, notifications.ErrNotMember) {
		t.Fatalf("err = %v", err)
	}
	if n := w.count("notification_email_prefs", "user_id = ?", w.bob.id) + w.count("notification_email_settings", "user_id = ?", w.bob.id); n != 0 {
		t.Fatalf("%d rows for someone who left", n)
	}
	// And a JSON null is not an update.
	rec := w.mallory.do(http.MethodPut, w.emailPath(), json.RawMessage("null"))
	expect(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), `"invalid_json"`) || w.count("notification_email_settings", "user_id = ?", w.mallory.id) != 0 {
		t.Fatal(rec.Body.String())
	}
}

func TestTheSummarysZoneIsTheDefaultUntilThePersonSetsTheTime(t *testing.T) {
	w := newMailWorld(t)
	if got := decode[notifications.EmailChoices](t, w.get(w.bob)); got.Digest.Chosen {
		t.Fatalf("digest = %+v", got.Digest)
	}
	// A choice of a kind alone doesn't set it.
	if got := w.put(w.bob, map[string]any{"kinds": map[string]string{notifications.KindCardAssigned: "instant"}}); got.Digest.Chosen {
		t.Fatalf("digest = %+v", got.Digest)
	}
	got := w.put(w.bob, map[string]any{"digest": map[string]any{"hour": 8, "timezone": "UTC"}})
	if !got.Digest.Chosen || got.Digest.Timezone != "UTC" {
		t.Fatalf("digest = %+v", got.Digest)
	}
	// A client can't set it by itself: it is an answer.
	if got := w.put(w.mallory, map[string]any{"digest": map[string]any{"hour": 8, "timezone": "UTC", "chosen": false}}); !got.Digest.Chosen {
		t.Fatalf("digest = %+v", got.Digest)
	}
}
