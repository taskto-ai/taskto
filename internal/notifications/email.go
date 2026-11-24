package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"taskto.ai/taskto/internal/mail"
	"taskto.ai/taskto/internal/schedule"
)

// Email is the third way to be told, after the inbox and Web Push, for the people who opt in. It
// is off for everyone until they choose, in a workspace, for each kind of notification:
//
//   - instant: a mail soon after the notification is made, one for each (email_deliveries, sent by
//     the push worker's loop with bounded tries);
//   - digest: one mail a day, at the hour of the person's own time zone, that counts what
//     happened since the last one (the general scheduler's job KindDigest, one per person and
//     workspace).
//
// A mail says what a push says and no more: the kind of thing that happened, in the person's
// language, and a link back to the app. It never holds a title, a name, a comment or an ID of a
// record. The digest says how many notifications are waiting and links to the inbox. What a mail
// may say is decided when it is sent, from what the person may read then, exactly as for a push:
// a notification of something they can no longer read sends nothing and is not counted.
//
// Nothing is sent unless the server can send mail (docs/install.md#email) and knows its own
// address for the links (the public URL): without them the choices can't be turned on, and the
// worker leaves what is queued alone until it is old.

// The modes of a kind's email choice. A kind with no choice is off.
const (
	ModeOff     = "off"
	ModeInstant = "instant"
	ModeDigest  = "digest"
)

// KindDigest is the scheduler's kind of the daily summary.
const KindDigest schedule.Kind = "notifications.digest"

// Email's bounds and timings.
const (
	// DefaultDigestHour is when the summary is sent unless the person chose.
	DefaultDigestHour = 8
	// DefaultTimezone is the time zone of a person who chose none.
	DefaultTimezone = "UTC"
	// emailTTL is how old a notification may be and still be mailed as it happens.
	emailTTL = 24 * time.Hour
	// digestWindow is how far back a summary looks: a notification older than that is not counted.
	digestWindow = 48 * time.Hour
	// maxInstantPerHour bounds the mails as they happen to one person: the rest stay in the inbox.
	maxInstantPerHour = 20
	emailMaxAttempts  = 5
	emailSendTimeout  = 30 * time.Second
	emailBackoffBase  = time.Minute
	emailBackoffMax   = time.Hour
)

// EmailKinds are the kinds a person may choose email for.
func EmailKinds() []string {
	return []string{KindCardAssigned, KindAppEffectFailed}
}

func emailKind(kind string) bool {
	for _, k := range EmailKinds() {
		if k == kind {
			return true
		}
	}
	return false
}

// EmailChoices are a person's email settings in one workspace.
type EmailChoices struct {
	// Available says whether the server can send mail at all, and links back to itself.
	Available bool `json:"available"`
	// Kinds says each kind's mode: off, instant or digest.
	Kinds  map[string]string `json:"kinds"`
	Digest EmailDigest       `json:"digest"`
}

// EmailDigest says when the daily summary is sent: the hour (0 to 23) of the time zone, an IANA name.
type EmailDigest struct {
	Hour     int    `json:"hour"`
	Timezone string `json:"timezone"`
}

// EmailUpdate is a change to a person's choices: what is nil stays.
type EmailUpdate struct {
	Kinds  map[string]string `json:"kinds"`
	Digest *EmailDigest      `json:"digest"`
}

// ErrEmailInvalid is a choice the server refuses.
var ErrEmailInvalid = errors.New("notifications: invalid email choice")

// ErrEmailUnavailable is turning email on where the server can't send it.
var ErrEmailUnavailable = errors.New("notifications: the server can't send email")

// emailReady says whether mail can be sent now: an SMTP server and the address links go to.
func (s *Service) emailReady() bool {
	return s.mail.Configured() && s.publicURL != ""
}

// AttachScheduler registers the daily summary's handler and lets the service schedule it. Call it
// before the scheduler starts.
func (s *Service) AttachScheduler(sch *schedule.Service) error {
	if err := sch.Register(KindDigest, s.sendDigest); err != nil {
		return err
	}
	s.sched = sch
	return nil
}

// EmailChoices returns what userID chose in the workspace.
func (s *Service) EmailChoices(ctx context.Context, userID, workspaceID int64) (EmailChoices, error) {
	out := EmailChoices{Available: s.emailReady(), Kinds: map[string]string{}, Digest: EmailDigest{Hour: DefaultDigestHour, Timezone: DefaultTimezone}}
	for _, k := range EmailKinds() {
		out.Kinds[k] = ModeOff
	}
	rows, err := s.db.QueryContext(ctx, `SELECT kind, mode FROM notification_email_prefs WHERE user_id = ? AND workspace_id = ?`, userID, workspaceID)
	if err != nil {
		return EmailChoices{}, err
	}
	for rows.Next() {
		var kind, mode string
		if err := rows.Scan(&kind, &mode); err != nil {
			rows.Close()
			return EmailChoices{}, err
		}
		if _, known := out.Kinds[kind]; known {
			out.Kinds[kind] = mode
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return EmailChoices{}, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT digest_hour, timezone FROM notification_email_settings WHERE user_id = ? AND workspace_id = ?`,
		userID, workspaceID).Scan(&out.Digest.Hour, &out.Digest.Timezone)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return EmailChoices{}, err
	}
	return out, nil
}

// SetEmailChoices changes what userID chose in the workspace, and schedules (or ends) the daily
// summary to match. Turning anything on needs a server that can send mail (ErrEmailUnavailable);
// turning things off never does. The caller has checked that userID is a member.
func (s *Service) SetEmailChoices(ctx context.Context, userID, workspaceID int64, u EmailUpdate) error {
	for kind, mode := range u.Kinds {
		if !emailKind(kind) || (mode != ModeOff && mode != ModeInstant && mode != ModeDigest) {
			return fmt.Errorf("%w: a kind is one of %s and its mode off, instant or digest", ErrEmailInvalid, strings.Join(EmailKinds(), ", "))
		}
		if mode != ModeOff && !s.emailReady() {
			return ErrEmailUnavailable
		}
	}
	if d := u.Digest; d != nil {
		if d.Hour < 0 || d.Hour > 23 || len(d.Timezone) == 0 || len(d.Timezone) > 64 {
			return fmt.Errorf("%w: the hour is 0 to 23 and the time zone an IANA name", ErrEmailInvalid)
		}
		if _, err := loadZone(d.Timezone); err != nil {
			return fmt.Errorf("%w: the time zone is an IANA name such as Europe/Lisbon", ErrEmailInvalid)
		}
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		had, err := s.usesDigest(ctx, tx, userID, workspaceID)
		if err != nil {
			return err
		}
		for kind, mode := range u.Kinds {
			if mode == ModeOff {
				_, err = tx.ExecContext(ctx, `DELETE FROM notification_email_prefs WHERE user_id = ? AND workspace_id = ? AND kind = ?`, userID, workspaceID, kind)
			} else {
				_, err = tx.ExecContext(ctx, `
					INSERT INTO notification_email_prefs (user_id, workspace_id, kind, mode) VALUES (?, ?, ?, ?)
					ON CONFLICT (user_id, workspace_id, kind) DO UPDATE SET mode = excluded.mode`, userID, workspaceID, kind, mode)
			}
			if err != nil {
				return err
			}
		}
		// A summary counts what comes after the choice: the cursor is where the notifications are now.
		hour, zone := DefaultDigestHour, DefaultTimezone
		if u.Digest != nil {
			hour, zone = u.Digest.Hour, u.Digest.Timezone
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO notification_email_settings (user_id, workspace_id, digest_hour, timezone, digest_cursor)
			VALUES (?, ?, ?, ?, (SELECT coalesce(max(id), 0) FROM notifications WHERE user_id = ? AND workspace_id = ?))
			ON CONFLICT (user_id, workspace_id) DO NOTHING`, userID, workspaceID, hour, zone, userID, workspaceID); err != nil {
			return err
		}
		if u.Digest != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_email_settings SET digest_hour = ?, timezone = ? WHERE user_id = ? AND workspace_id = ?`,
				hour, zone, userID, workspaceID); err != nil {
				return err
			}
		}
		now, err := s.usesDigest(ctx, tx, userID, workspaceID)
		if err != nil {
			return err
		}
		if now && !had {
			if _, err := tx.ExecContext(ctx, `
				UPDATE notification_email_settings
				SET digest_cursor = (SELECT coalesce(max(id), 0) FROM notifications WHERE user_id = ? AND workspace_id = ?)
				WHERE user_id = ? AND workspace_id = ?`, userID, workspaceID, userID, workspaceID); err != nil {
				return err
			}
		}
		if s.sched == nil {
			return nil
		}
		if !now {
			_, err := s.sched.Cancel(ctx, tx, userID, KindDigest, digestKey(workspaceID))
			if errors.Is(err, schedule.ErrNotFound) {
				err = nil
			}
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT digest_hour, timezone FROM notification_email_settings WHERE user_id = ? AND workspace_id = ?`,
			userID, workspaceID).Scan(&hour, &zone); err != nil {
			return err
		}
		at, err := loadZone(zone)
		if err != nil {
			return err
		}
		_, err = s.sched.Reschedule(ctx, tx, schedule.Job{
			Kind: KindDigest, UserID: userID, WorkspaceID: workspaceID, DedupeKey: digestKey(workspaceID),
			RunAt: nextDigest(s.clock(), hour, at), Every: 24 * time.Hour,
		})
		return err
	})
}

func digestKey(workspaceID int64) string { return "workspace:" + strconv.FormatInt(workspaceID, 10) }

func loadZone(name string) (*time.Location, error) {
	if name == "Local" {
		return nil, errors.New("not an IANA name")
	}
	return time.LoadLocation(name)
}

// nextDigest is the next time after now that it is hour o'clock in loc.
func nextDigest(now time.Time, hour int, loc *time.Location) time.Time {
	local := now.In(loc)
	at := time.Date(local.Year(), local.Month(), local.Day(), hour, 0, 0, 0, loc)
	if !at.After(now) {
		at = time.Date(local.Year(), local.Month(), local.Day()+1, hour, 0, 0, 0, loc)
	}
	return at.UTC()
}

func (s *Service) usesDigest(ctx context.Context, tx *sql.Tx, userID, workspaceID int64) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM notification_email_prefs WHERE user_id = ? AND workspace_id = ? AND mode = 'digest'`, userID, workspaceID).Scan(&n)
	return n > 0, err
}

// recipientOf is who a mail goes to and in which language: the person's own, else their workspace's.
func (s *Service) recipientOf(ctx context.Context, userID, workspaceID int64) (address, locale string, err error) {
	var userLocale, workspaceLocale string
	err = s.db.QueryRowContext(ctx, `
		SELECT u.email, u.locale, w.locale FROM users u, workspaces w
		WHERE u.id = ? AND w.id = ? AND EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = u.id AND m.workspace_id = w.id)`,
		userID, workspaceID).Scan(&address, &userLocale, &workspaceLocale)
	if userLocale != "" {
		return address, userLocale, err
	}
	return address, workspaceLocale, err
}

// sendDigest is the handler of the daily summary, run as the person it is for: it counts their
// unread notifications of the kinds they chose for the summary that came after the last one and
// that they may still read, and mails the count with a link to the inbox. Nothing is sent when
// there is none, or when the server can't send mail.
func (s *Service) sendDigest(ctx context.Context, job schedule.Job) error {
	if !s.emailReady() {
		return nil
	}
	var cursor int64
	err := s.db.QueryRowContext(ctx, `SELECT digest_cursor FROM notification_email_settings WHERE user_id = ? AND workspace_id = ?`,
		job.UserID, job.WorkspaceID).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+storedColumns+` FROM notifications
		WHERE user_id = ? AND workspace_id = ? AND id > ? AND read_at IS NULL AND created_at > ?
		  AND kind IN (SELECT kind FROM notification_email_prefs WHERE user_id = ? AND workspace_id = ? AND mode = 'digest')
		ORDER BY id`,
		job.UserID, job.WorkspaceID, cursor, s.stamp(s.clock().Add(-digestWindow)), job.UserID, job.WorkspaceID)
	if err != nil {
		return err
	}
	var found []stored
	for rows.Next() {
		n, err := scanStored(rows)
		if err != nil {
			rows.Close()
			return err
		}
		found = append(found, n)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(found) == 0 {
		return nil
	}
	// Only what the person may read now is counted, and so told.
	reader := s.readerFor(job.UserID)
	count := 0
	for _, n := range found {
		it, err := reader.item(ctx, n)
		if err != nil {
			return err
		}
		if it.Kind != KindChanged {
			count++
		}
	}
	if count > 0 {
		address, locale, err := s.recipientOf(ctx, job.UserID, job.WorkspaceID)
		if err != nil {
			return schedule.Permanent(err)
		}
		sendCtx, cancel := context.WithTimeout(ctx, emailSendTimeout)
		defer cancel()
		if err := s.mail.SendTemplate(sendCtx, address, locale, "digest", map[string]string{
			"count":      strconv.Itoa(count),
			"action_url": s.publicURL + "/w/" + strconv.FormatInt(job.WorkspaceID, 10) + "/notifications",
		}); err != nil {
			cause, permanent := mailFailure(err)
			switch {
			case cause == nil:
				return nil
			case permanent:
				return schedule.Permanent(cause)
			}
			return cause
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE notification_email_settings SET digest_cursor = ? WHERE user_id = ? AND workspace_id = ? AND digest_cursor < ?`,
		found[len(found)-1].id, job.UserID, job.WorkspaceID, found[len(found)-1].id)
	return err
}

// mailFailure says what to make of a failed send: the cause to keep and log, which holds the stage
// and the server's reply code and never its words (they can name hosts and addresses), whether
// trying again can't help, and nil for a server that has no mail any more.
func mailFailure(err error) (cause error, permanent bool) {
	var se *mail.SendError
	switch {
	case errors.Is(err, mail.ErrNotConfigured):
		return nil, false
	case errors.As(err, &se):
		slog.Warn("notifications: an email wasn't sent", "stage", se.Stage, "smtp_code", se.Code)
		return fmt.Errorf("mail: %s: %d", se.Stage, se.Code), se.Stage == mail.StageRejected && se.Code >= 500
	}
	slog.Warn("notifications: an email couldn't be made", "err", err)
	return err, false
}

// emailDelivery is a queued mail.
type emailDelivery struct {
	id, notificationID int64
	attempts           int
}

// DrainEmailOnce sends the mails that are due now and returns when none is: for tests, which have
// no worker.
func (s *Service) DrainEmailOnce(ctx context.Context) { s.drainEmail(ctx) }

// drainEmail sends the queued mails that are due, one after another. Without a server that can send
// mail it leaves them as they are.
func (s *Service) drainEmail(ctx context.Context) {
	for ctx.Err() == nil && s.emailReady() {
		d, found, err := s.claimEmail(ctx)
		if err == nil && found {
			err = s.runEmail(ctx, d)
		}
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("notifications: email worker", "err", err)
			}
			return
		}
		if !found {
			return
		}
	}
}

// claimEmail takes the next mail that is due, counting the try and moving the next one out, as
// claim does for pushes; a mail that has used all its tries is given up.
func (s *Service) claimEmail(ctx context.Context) (emailDelivery, bool, error) {
	t := s.clock()
	if _, err := s.db.ExecContext(ctx, `
		UPDATE email_deliveries SET state = 'failed', updated_at = ?
		WHERE state = 'pending' AND attempts >= ? AND next_attempt_at <= ?`, s.stamp(t), emailMaxAttempts, s.stamp(t)); err != nil {
		return emailDelivery{}, false, err
	}
	var d emailDelivery
	err := s.db.QueryRowContext(ctx, `
		UPDATE email_deliveries SET attempts = attempts + 1, next_attempt_at = ?, updated_at = ?
		WHERE id = (SELECT id FROM email_deliveries WHERE state = 'pending' AND attempts < ? AND next_attempt_at <= ? ORDER BY id LIMIT 1)
		RETURNING id, notification_id, attempts`,
		s.stamp(t.Add(2*emailSendTimeout)), s.stamp(t), emailMaxAttempts, s.stamp(t)).Scan(&d.id, &d.notificationID, &d.attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return emailDelivery{}, false, nil
	}
	return d, err == nil, err
}

func (s *Service) settleEmail(ctx context.Context, d emailDelivery, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE email_deliveries SET state = ?, updated_at = ? WHERE id = ?`, state, s.now(), d.id)
	return err
}

// runEmail makes one try: it drops what must not be mailed (read meanwhile, old, no longer chosen,
// the person gone from the workspace, something they may no longer read, too many in the hour) and
// otherwise sends the kind's message, with a link to the notification.
func (s *Service) runEmail(ctx context.Context, d emailDelivery) error {
	var n stored
	var ref string
	var mode sql.NullString
	var member bool
	err := s.db.QueryRowContext(ctx, `
		SELECT n.id, n.user_id, n.workspace_id, n.kind, n.ref, n.created_at, n.read_at IS NOT NULL,
			(SELECT mode FROM notification_email_prefs p WHERE p.user_id = n.user_id AND p.workspace_id = n.workspace_id AND p.kind = n.kind),
			EXISTS (SELECT 1 FROM memberships m WHERE m.workspace_id = n.workspace_id AND m.user_id = n.user_id)
		FROM notifications n WHERE n.id = ?`, d.notificationID).Scan(&n.id, &n.userID, &n.workspaceID, &n.kind, &ref, &n.createdAt, &n.read, &mode, &member)
	if err != nil {
		return s.settleEmail(ctx, d, "failed") // the notification is gone
	}
	if err := json.Unmarshal([]byte(ref), &n.ref); err != nil {
		return s.settleEmail(ctx, d, "failed")
	}
	created, perr := time.Parse(timeFormat, n.createdAt)
	if n.read || !member || mode.String != ModeInstant || perr != nil || s.clock().Sub(created) > emailTTL || !emailKind(n.kind) {
		return s.settleEmail(ctx, d, "failed")
	}
	var recent int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM email_deliveries e JOIN notifications x ON x.id = e.notification_id
		WHERE x.user_id = ? AND e.state = 'sent' AND e.updated_at > ?`, n.userID, s.stamp(s.clock().Add(-time.Hour))).Scan(&recent); err != nil {
		return err
	}
	if recent >= maxInstantPerHour {
		return s.settleEmail(ctx, d, "failed")
	}
	it, err := s.readerFor(n.userID).item(ctx, n)
	if err != nil {
		return s.retryEmail(ctx, d, err)
	}
	if it.Kind == KindChanged {
		return s.settleEmail(ctx, d, "failed")
	}
	address, locale, err := s.recipientOf(ctx, n.userID, n.workspaceID)
	if err != nil {
		return s.settleEmail(ctx, d, "failed")
	}
	sendCtx, cancel := context.WithTimeout(ctx, emailSendTimeout)
	defer cancel()
	err = s.mail.SendTemplate(sendCtx, address, locale, "notification_"+n.kind, map[string]string{
		"action_url": s.publicURL + "/notification/" + strconv.FormatInt(n.id, 10),
	})
	if err == nil {
		return s.settleEmail(ctx, d, "sent")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	cause, permanent := mailFailure(err)
	if cause == nil || permanent {
		return s.settleEmail(ctx, d, "failed")
	}
	return s.retryEmail(ctx, d, cause)
}

// retryEmail moves a failed try's next one out, or gives the mail up after its last.
func (s *Service) retryEmail(ctx context.Context, d emailDelivery, cause error) error {
	if d.attempts >= emailMaxAttempts {
		return s.settleEmail(ctx, d, "failed")
	}
	after := min(emailBackoffBase<<(2*(d.attempts-1)), emailBackoffMax)
	slog.Warn("notifications: an email will be tried again", "delivery", d.id, "attempt", d.attempts, "err", cause)
	_, err := s.db.ExecContext(ctx, `UPDATE email_deliveries SET next_attempt_at = ?, updated_at = ? WHERE id = ?`, s.stamp(s.clock().Add(after)), s.now(), d.id)
	return err
}
