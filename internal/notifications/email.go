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
//     the email worker, a loop of its own so a slow SMTP server can't hold back a push);
//   - digest: one mail a day, at the hour of the person's own time zone, that counts what
//     happened since the last one (the general scheduler's job KindDigest, one per person and
//     workspace; each run schedules the next from the local date, so daylight saving time
//     moves nothing).
//
// # At most once
//
// A mail that went out twice is worse than one that never did, since the inbox has it either way.
// Before a message is submitted to the relay, the attempt is written to the ledger
// (notification_email_ledger) under a key that names what it is for: the notification for an
// instant mail, the job's occurrence (schedule.Job.Occurrence) for a summary. A key with a row is
// never submitted again, except when the relay definitely refused it before taking it (a reply
// code, a refused connection or sign-in): then and only then a later try may send it. When the
// outcome is unknown (the process stopped with the attempt written and no result, or the
// connection was lost after the message was sent: mail.SendError.Ambiguous) the mail is counted
// as sent and never sent again. The same ledger is what the hourly limit counts, so the limit
// holds for concurrent workers and survives the notifications it counted being deleted.
//
// What may be mailed is checked again at the last point before the submission that can still
// cancel it: the person's choice, their address's confirmation, their membership, their access to
// what the notification is about and, for a summary, that the job is still this occurrence.
//
// # Confirmed addresses
//
// Nothing is mailed to an address until its owner has confirmed it (emailverify.go).
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
	// digestTries is how many times a run of the summary is tried when sending it fails for a reason
	// that may pass; after them what is waiting is left for the next summary.
	digestTries = 3
)

// EmailKinds are the kinds a person may choose email for.
func EmailKinds() []string {
	return []string{KindCardAssigned, KindCardComment, KindCardMention, KindCardDue, KindCardOverdue, KindAppEffectFailed}
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
	// Address is where mail would go and whether its owner has confirmed it: nothing is sent
	// before.
	Address EmailAddress `json:"address"`
	// Kinds says each kind's mode: off, instant or digest.
	Kinds  map[string]string `json:"kinds"`
	Digest EmailDigest       `json:"digest"`
}

// EmailDigest says when the daily summary is sent: the hour (0 to 23) of the time zone, an IANA name.
type EmailDigest struct {
	Hour     int    `json:"hour"`
	Timezone string `json:"timezone"`
	// Chosen says the person has set the time themselves, so their zone, UTC included, is theirs;
	// before, it is the default, which the settings page may replace with the browser's. Answers
	// only: a change of it is ignored.
	Chosen bool `json:"chosen"`
}

// EmailUpdate is a change to a person's choices: what is nil stays.
type EmailUpdate struct {
	Kinds  map[string]string `json:"kinds"`
	Digest *EmailDigest      `json:"digest"`
}

// ErrEmailInvalid is a choice the server refuses.
var ErrEmailInvalid = errors.New("notifications: invalid email choice")

// ErrNotMember is a change for someone who left the workspace meanwhile.
var ErrNotMember = errors.New("notifications: not a member of the workspace")

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
	var err error
	if out.Address, err = s.addressOf(ctx, s.db, userID); err != nil {
		return EmailChoices{}, err
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
	err = s.db.QueryRowContext(ctx, `SELECT digest_hour, timezone, timezone_chosen FROM notification_email_settings WHERE user_id = ? AND workspace_id = ?`,
		userID, workspaceID).Scan(&out.Digest.Hour, &out.Digest.Timezone, &out.Digest.Chosen)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return EmailChoices{}, err
	}
	return out, nil
}

// SetEmailChoices changes what userID chose in the workspace, and schedules (or ends) the daily
// summary to match. Turning anything on needs a server that can send mail (ErrEmailUnavailable);
// turning things off never does. The first time anything is turned on for an address that isn't
// confirmed, a link to confirm it is mailed (emailverify.go), and nothing else is sent until it is
// opened. The caller has checked that userID is a member.
func (s *Service) SetEmailChoices(ctx context.Context, userID, workspaceID int64, u EmailUpdate) error {
	turnsOn := false
	for kind, mode := range u.Kinds {
		if !emailKind(kind) || (mode != ModeOff && mode != ModeInstant && mode != ModeDigest) {
			return fmt.Errorf("%w: a kind is one of %s and its mode off, instant or digest", ErrEmailInvalid, strings.Join(EmailKinds(), ", "))
		}
		if mode != ModeOff {
			if !s.emailReady() {
				return ErrEmailUnavailable
			}
			turnsOn = true
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
	err := s.write(ctx, func(tx *sql.Tx) error {
		// Membership is checked again here: leaving deletes the person's choices in the same
		// write lock, so a choice can't be stored for someone who has just left.
		var member bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE user_id = ? AND workspace_id = ?)`,
			userID, workspaceID).Scan(&member); err != nil {
			return err
		}
		if !member {
			return ErrNotMember
		}
		var prevHour int
		var prevZone string
		err := tx.QueryRowContext(ctx, `SELECT digest_hour, timezone FROM notification_email_settings WHERE user_id = ? AND workspace_id = ?`,
			userID, workspaceID).Scan(&prevHour, &prevZone)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		known := err == nil
		for kind, mode := range u.Kinds {
			var err error
			if mode == ModeOff {
				_, err = tx.ExecContext(ctx, `DELETE FROM notification_email_prefs WHERE user_id = ? AND workspace_id = ? AND kind = ?`, userID, workspaceID, kind)
			} else {
				// since is where the notifications are when a kind turns to a summary: it counts what
				// comes after, never what was already there or was mailed as it happened.
				_, err = tx.ExecContext(ctx, `
					INSERT INTO notification_email_prefs (user_id, workspace_id, kind, mode, since)
					VALUES (?, ?, ?, ?, (SELECT coalesce(max(id), 0) FROM notifications WHERE user_id = ? AND workspace_id = ?))
					ON CONFLICT (user_id, workspace_id, kind) DO UPDATE SET mode = excluded.mode,
						since = CASE WHEN notification_email_prefs.mode = 'digest' AND excluded.mode = 'digest'
							THEN notification_email_prefs.since ELSE excluded.since END`,
					userID, workspaceID, kind, mode, userID, workspaceID)
			}
			if err != nil {
				return err
			}
		}
		hour, zone := DefaultDigestHour, DefaultTimezone
		if u.Digest != nil {
			hour, zone = u.Digest.Hour, u.Digest.Timezone
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO notification_email_settings (user_id, workspace_id, digest_hour, timezone, digest_cursor)
			VALUES (?, ?, ?, ?, 0) ON CONFLICT (user_id, workspace_id) DO NOTHING`, userID, workspaceID, hour, zone); err != nil {
			return err
		}
		if u.Digest != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_email_settings SET digest_hour = ?, timezone = ?, timezone_chosen = 1 WHERE user_id = ? AND workspace_id = ?`,
				hour, zone, userID, workspaceID); err != nil {
				return err
			}
		}
		if s.sched == nil {
			return nil
		}
		now, err := s.usesDigest(ctx, tx, userID, workspaceID)
		if err != nil {
			return err
		}
		if !now {
			_, err := s.sched.Cancel(ctx, tx, userID, KindDigest, digestKey(workspaceID))
			if errors.Is(err, schedule.ErrNotFound) {
				err = nil
			}
			return err
		}
		// A change that leaves the time as it was leaves the waiting occurrence alone: saving
		// another choice must not push today's summary to tomorrow.
		moved := u.Digest != nil && known && (u.Digest.Hour != prevHour || u.Digest.Timezone != prevZone)
		if job, err := s.sched.Find(ctx, tx, userID, KindDigest, digestKey(workspaceID)); err == nil && job.State == schedule.StatePending && !moved {
			return nil
		} else if err != nil && !errors.Is(err, schedule.ErrNotFound) {
			return err
		}
		return s.scheduleDigest(ctx, tx, userID, workspaceID)
	})
	if err != nil || !turnsOn {
		return err
	}
	// The first choice for an address that isn't confirmed sends the link that confirms it. A link
	// that can't be sent doesn't undo the choice: the settings say the address is unconfirmed, and
	// ask for another.
	if a, err := s.addressOf(ctx, s.db, userID); err == nil && a.State == AddressUnverified {
		if err := s.SendVerification(ctx, userID, workspaceID); err != nil {
			var soon *TooSoonError
			if !errors.As(err, &soon) {
				slog.Warn("notifications: no confirmation link was sent", "err", err)
			}
		}
	}
	return nil
}

func digestKey(workspaceID int64) string { return "workspace:" + strconv.FormatInt(workspaceID, 10) }

// scheduleDigest sets the person's summary to run at the next hour o'clock of their own time zone.
// There is one job and no fixed interval: each run schedules the next (nextDigestRun) from the local
// date, so the summary stays at the hour across the change to and from daylight saving time.
func (s *Service) scheduleDigest(ctx context.Context, q schedule.DBTX, userID, workspaceID int64) error {
	var hour int
	var zone string
	if err := q.QueryRowContext(ctx, `SELECT digest_hour, timezone FROM notification_email_settings WHERE user_id = ? AND workspace_id = ?`,
		userID, workspaceID).Scan(&hour, &zone); err != nil {
		return err
	}
	loc, err := loadZone(zone)
	if err != nil {
		return err
	}
	_, err = s.sched.Reschedule(ctx, q, schedule.Job{
		Kind: KindDigest, UserID: userID, WorkspaceID: workspaceID, DedupeKey: digestKey(workspaceID),
		RunAt: nextDigest(s.clock(), hour, loc),
	})
	return err
}

func loadZone(name string) (*time.Location, error) {
	if name == "Local" {
		return nil, errors.New("not an IANA name")
	}
	return time.LoadLocation(name)
}

// nextDigest is the next time after now that it is hour o'clock in loc, from the local date: the
// day after today is the date plus one, however long that day is.
//
// An hour that doesn't exist on a day (the clocks skip it) is the first moment after the gap, and one
// that happens twice (they go back) is its first time.
func nextDigest(now time.Time, hour int, loc *time.Location) time.Time {
	local := now.In(loc)
	at := localHour(local.Year(), local.Month(), local.Day(), hour, loc)
	if !at.After(now) {
		at = localHour(local.Year(), local.Month(), local.Day()+1, hour, loc)
	}
	return at.UTC()
}

// localHour is hour o'clock of a date in loc. time.Date says an hour inside a gap is one of two
// moments around it, whichever it picks; this is always the later: the first moment the wall clock
// shows the hour or more.
func localHour(year int, month time.Month, day, hour int, loc *time.Location) time.Time {
	at := time.Date(year, month, day, hour, 0, 0, 0, loc)
	want := time.Date(year, month, day, hour, 0, 0, 0, time.UTC)
	wall := func(t time.Time) time.Time {
		y, m, d := t.In(loc).Date()
		h, mi, sec := t.In(loc).Clock()
		return time.Date(y, m, d, h, mi, sec, 0, time.UTC)
	}
	// Back to before the gap if the pick was after it (so the walk below finds its start), then
	// forward until the wall clock has reached the hour.
	for i := 0; i < 8 && wall(at.Add(-15*time.Minute)).Compare(want) >= 0; i++ {
		at = at.Add(-15 * time.Minute)
	}
	for i := 0; i < 4*48 && wall(at).Before(want); i++ {
		at = at.Add(15 * time.Minute)
	}
	return at
}

func (s *Service) usesDigest(ctx context.Context, q queryer, userID, workspaceID int64) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM notification_email_prefs WHERE user_id = ? AND workspace_id = ? AND mode = 'digest'`, userID, workspaceID).Scan(&n)
	return n > 0, err
}

// recipientOf is who a mail goes to and in which language: the person's own, else their workspace's.
func (s *Service) recipientOf(ctx context.Context, q queryer, userID, workspaceID int64) (address, locale string, err error) {
	var userLocale, workspaceLocale string
	err = q.QueryRowContext(ctx, `
		SELECT u.email, u.locale, w.locale FROM users u, workspaces w
		WHERE u.id = ? AND w.id = ? AND EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = u.id AND m.workspace_id = w.id)`,
		userID, workspaceID).Scan(&address, &userLocale, &workspaceLocale)
	if userLocale != "" {
		return address, userLocale, err
	}
	return address, workspaceLocale, err
}

// The ledger's states (notification_email_ledger).
const (
	ledgerSending   = "sending"
	ledgerSent      = "sent"
	ledgerRefused   = "refused"
	ledgerUnknown   = "unknown"
	ledgerCancelled = "cancelled"
)

// slot is what reserving a mail's place in the ledger came to.
type slot int

const (
	// slotReserved: the caller may submit the mail, and must record the outcome.
	slotReserved slot = iota
	// slotDone: the mail was submitted before (or may have been): it is never submitted again.
	slotDone
	// slotFull: the person has had the most instant mails in the hour.
	slotFull
)

// reserve writes the attempt to key's row, in one transaction with the count the hourly limit
// takes, before anything is submitted. An existing row decides: a mail that was sent, or whose
// outcome is unknown (a try that was started and never recorded one is that) is done, with the
// newest notification it covered; one the relay refused or that was cancelled gets a new try.
func (s *Service) reserve(ctx context.Context, userID int64, key, kind string, upto int64) (slot, int64, error) {
	out, covered := slotReserved, upto
	err := s.write(ctx, func(tx *sql.Tx) error {
		out, covered = slotReserved, upto
		now := s.now()
		var state string
		var prev int64
		err := tx.QueryRowContext(ctx, `SELECT state, upto FROM notification_email_ledger WHERE key = ?`, key).Scan(&state, &prev)
		switch {
		case err == nil && (state == ledgerSent || state == ledgerUnknown):
			out, covered = slotDone, prev
			return nil
		case err == nil && state == ledgerSending:
			// A worker wrote the attempt and died without a result: the relay may have taken it.
			out, covered = slotDone, prev
			_, err = tx.ExecContext(ctx, `UPDATE notification_email_ledger SET state = ?, updated_at = ? WHERE key = ?`, ledgerUnknown, now, key)
			return err
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if kind == "instant" {
			var recent int
			if err := tx.QueryRowContext(ctx, `
				SELECT count(*) FROM notification_email_ledger
				WHERE user_id = ? AND kind = 'instant' AND state IN ('sending', 'sent', 'unknown') AND reserved_at > ?`,
				userID, s.stamp(s.clock().Add(-time.Hour))).Scan(&recent); err != nil {
				return err
			}
			if recent >= maxInstantPerHour {
				out = slotFull
				return nil
			}
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO notification_email_ledger (user_id, key, kind, state, upto, attempts, reserved_at, updated_at)
			VALUES (?, ?, ?, 'sending', ?, 1, ?, ?)
			ON CONFLICT (key) DO UPDATE SET state = 'sending', upto = excluded.upto, attempts = attempts + 1,
				reserved_at = excluded.reserved_at, updated_at = excluded.updated_at`, userID, key, kind, upto, now, now)
		return err
	})
	return out, covered, err
}

// settleLedger records what came of the attempt under key. It is written even when the server is
// stopping, since an attempt with no outcome is read as "may have been sent".
func (s *Service) settleLedger(ctx context.Context, key, state string, upto int64) error {
	_, err := s.db.ExecContext(context.WithoutCancel(ctx), `
		UPDATE notification_email_ledger SET state = ?, upto = ?, updated_at = ? WHERE key = ? AND state = 'sending'`, state, upto, s.now(), key)
	return err
}

// submission is how a send came out, for the ledger and for what to do next.
type submission struct {
	// state is the ledger's: sent, unknown (it may have been taken), refused (it was not) or
	// cancelled (the server has no mail any more).
	state string
	// permanent: sending again can't help.
	permanent bool
	cause     error
}

// submitted classifies the result of mail.Service.Send. A failure of the relay's answer to the
// message itself is the one case in which it is not known whether the mail was taken.
func submitted(err error) submission {
	var se *mail.SendError
	switch {
	case err == nil:
		return submission{state: ledgerSent}
	case errors.Is(err, mail.ErrNotConfigured):
		return submission{state: ledgerCancelled}
	case errors.As(err, &se):
		slog.Warn("notifications: an email wasn't sent", "stage", se.Stage, "smtp_code", se.Code, "ambiguous", se.Ambiguous)
		// The reply's code and the stage, never its words: they can name hosts and addresses.
		cause := fmt.Errorf("mail: %s: %d", se.Stage, se.Code)
		if se.Ambiguous {
			return submission{state: ledgerUnknown, permanent: true, cause: cause}
		}
		return submission{state: ledgerRefused, permanent: se.Stage == mail.StageRejected && se.Code >= 500, cause: cause}
	}
	// Nothing reached the relay: the message could not be made.
	slog.Warn("notifications: an email couldn't be made", "err", err)
	return submission{state: ledgerRefused, cause: err}
}

// digestPlan is what a summary would say now.
type digestPlan struct {
	count           int
	upto            int64
	address, locale string
}

// digestPlanFor works out the summary of job's person: the unread notifications of the kinds they
// chose for it, that came after the summary's cursor and after the kind was turned to a summary,
// and that they may still read. ok is false when there is nothing to say, or nothing may be sent:
// no mail, an unconfirmed address, no longer a member.
func (s *Service) digestPlanFor(ctx context.Context, job schedule.Job) (p digestPlan, ok bool, err error) {
	if !s.emailReady() {
		return p, false, nil
	}
	if confirmed, err := s.verified(ctx, s.db, job.UserID); err != nil || !confirmed {
		return p, false, err
	}
	var cursor int64
	err = s.db.QueryRowContext(ctx, `SELECT digest_cursor FROM notification_email_settings WHERE user_id = ? AND workspace_id = ?`,
		job.UserID, job.WorkspaceID).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+storedColumns+` FROM notifications
		WHERE user_id = ? AND workspace_id = ? AND id > ? AND read_at IS NULL AND created_at > ?
		  AND EXISTS (SELECT 1 FROM notification_email_prefs p
		              WHERE p.user_id = notifications.user_id AND p.workspace_id = notifications.workspace_id
		                AND p.kind = notifications.kind AND p.mode = 'digest' AND notifications.id > p.since)
		ORDER BY id`,
		job.UserID, job.WorkspaceID, cursor, s.stamp(s.clock().Add(-digestWindow)))
	if err != nil {
		return p, false, err
	}
	var found []stored
	for rows.Next() {
		n, err := scanStored(rows)
		if err != nil {
			rows.Close()
			return p, false, err
		}
		found = append(found, n)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return p, false, err
	}
	if len(found) == 0 {
		return p, false, nil
	}
	// Only what the person may read now is counted, and so told.
	reader := s.readerFor(job.UserID)
	for _, n := range found {
		it, err := reader.item(ctx, n)
		if err != nil {
			return p, false, err
		}
		if it.Kind != KindChanged {
			p.count++
		}
	}
	p.upto = found[len(found)-1].id
	if p.count == 0 {
		// Nothing to tell, and nothing to count again: they are passed over.
		return p, false, s.advanceDigest(ctx, job, p.upto)
	}
	p.address, p.locale, err = s.recipientOf(ctx, s.db, job.UserID, job.WorkspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (s *Service) advanceDigest(ctx context.Context, job schedule.Job, upto int64) error {
	_, err := s.db.ExecContext(context.WithoutCancel(ctx), `
		UPDATE notification_email_settings SET digest_cursor = ? WHERE user_id = ? AND workspace_id = ? AND digest_cursor < ?`,
		upto, job.UserID, job.WorkspaceID, upto)
	return err
}

// current says whether job is still the occurrence of its person's summary: it is not once the
// person has turned the summary off or changed its time (which schedules it anew).
func (s *Service) current(ctx context.Context, q schedule.DBTX, job schedule.Job) (bool, error) {
	if s.sched == nil {
		return false, nil
	}
	found, err := s.sched.Find(ctx, q, job.UserID, KindDigest, job.DedupeKey)
	if errors.Is(err, schedule.ErrNotFound) {
		return false, nil
	}
	return err == nil && found.Occurrence() == job.Occurrence(), err
}

// sendDigest is the handler of the daily summary, run as the person it is for: it counts their
// unread notifications of the kinds they chose for the summary that came after the last one and
// that they may still read, and mails the count with a link to the inbox, at most once for the
// occurrence however many times it is tried. Nothing is sent when there is none, when the server
// can't send mail or when the address isn't confirmed. Whatever comes of it, the next summary is
// scheduled, at the same hour of the person's own day.
func (s *Service) sendDigest(ctx context.Context, job schedule.Job) error {
	err := s.digestOnce(ctx, job)
	if err != nil && ctx.Err() == nil && job.Attempts < digestTries {
		return err // tried again, with the scheduler's waits
	}
	if err != nil {
		slog.Warn("notifications: a summary was left for the next one", "job", job.ID, "err", err)
	}
	return s.digestNext(ctx, job)
}

// digestNext schedules the person's next summary, unless this occurrence has been replaced.
func (s *Service) digestNext(ctx context.Context, job schedule.Job) error {
	// On a context of its own: the handler's may have ended, and the chain of summaries must not.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), emailSendTimeout)
	defer cancel()
	return s.write(ctx, func(tx *sql.Tx) error {
		if ok, err := s.current(ctx, tx, job); err != nil || !ok {
			return err
		}
		if on, err := s.usesDigest(ctx, tx, job.UserID, job.WorkspaceID); err != nil || !on {
			return err
		}
		return s.scheduleDigest(ctx, tx, job.UserID, job.WorkspaceID)
	})
}

func (s *Service) digestOnce(ctx context.Context, job schedule.Job) error {
	plan, ok, err := s.digestPlanFor(ctx, job)
	if err != nil || !ok {
		return err
	}
	key := "digest:" + job.Occurrence()
	got, covered, err := s.reserve(ctx, job.UserID, key, "digest", plan.upto)
	if err != nil {
		return err
	}
	if got == slotDone {
		// An earlier try of this occurrence submitted it, or died trying: it is not sent again, and
		// what it covered is not counted again.
		return s.advanceDigest(ctx, job, covered)
	}
	// The last point that can still cancel it: the occurrence, the choices, the address and the
	// person's access are asked again.
	if s.afterReserve != nil {
		s.afterReserve()
	}
	if cur, err := s.current(ctx, s.db, job); err != nil || !cur {
		return errors.Join(err, s.settleLedger(ctx, key, ledgerCancelled, plan.upto))
	}
	plan, ok, err = s.digestPlanFor(ctx, job)
	if err == nil && ok {
		// The plan takes a moment: the occurrence is asked once more before anything is submitted.
		ok, err = s.current(ctx, s.db, job)
	}
	if err != nil || !ok {
		return errors.Join(err, s.settleLedger(ctx, key, ledgerCancelled, plan.upto))
	}
	// What the message covers is on the ledger before it is submitted, so an attempt that stops
	// after the relay took it advances the cursor over everything it counted.
	if _, err := s.db.ExecContext(ctx, `UPDATE notification_email_ledger SET upto = ?, updated_at = ? WHERE key = ? AND state = 'sending'`,
		plan.upto, s.now(), key); err != nil {
		return errors.Join(err, s.settleLedger(ctx, key, ledgerRefused, plan.upto))
	}
	sendCtx, cancel := context.WithTimeout(ctx, emailSendTimeout)
	defer cancel()
	res := submitted(s.mail.SendTemplate(sendCtx, plan.address, plan.locale, "digest", map[string]string{
		"count":      strconv.Itoa(plan.count),
		"action_url": s.publicURL + "/w/" + strconv.FormatInt(job.WorkspaceID, 10) + "/notifications",
	}))
	if err := s.settleLedger(ctx, key, res.state, plan.upto); err != nil {
		return err
	}
	switch {
	case res.state == ledgerSent || res.state == ledgerUnknown:
		return s.advanceDigest(ctx, job, plan.upto)
	case res.state == ledgerCancelled || res.permanent:
		return nil
	}
	return res.cause
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
	_, err := s.db.ExecContext(context.WithoutCancel(ctx), `UPDATE email_deliveries SET state = ?, updated_at = ? WHERE id = ?`, state, s.now(), d.id)
	return err
}

// instantTarget is a notification that may be mailed now, and where.
type instantTarget struct {
	userID          int64
	kind            string
	address, locale string
}

// instantEligible decides whether the mail of notification id may go out now: it drops what must not
// be mailed (gone, read meanwhile, old, no longer chosen, an address that isn't confirmed, the person
// gone from the workspace, something they may no longer read). An error is a failure to find out.
func (s *Service) instantEligible(ctx context.Context, id int64) (instantTarget, bool, error) {
	var t instantTarget
	var n stored
	var ref string
	var mode sql.NullString
	var member bool
	err := s.db.QueryRowContext(ctx, `
		SELECT n.id, n.user_id, n.workspace_id, n.kind, n.ref, n.created_at, n.read_at IS NOT NULL,
			(SELECT mode FROM notification_email_prefs p WHERE p.user_id = n.user_id AND p.workspace_id = n.workspace_id AND p.kind = n.kind),
			EXISTS (SELECT 1 FROM memberships m WHERE m.workspace_id = n.workspace_id AND m.user_id = n.user_id)
		FROM notifications n WHERE n.id = ?`, id).Scan(&n.id, &n.userID, &n.workspaceID, &n.kind, &ref, &n.createdAt, &n.read, &mode, &member)
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil // the notification is gone
	}
	if err != nil {
		return t, false, err
	}
	created, readable := readStored(ref, &n)
	if !readable || n.read || !member || mode.String != ModeInstant || s.clock().Sub(created) > emailTTL || !emailKind(n.kind) || !s.emailReady() {
		return t, false, nil
	}
	if confirmed, err := s.verified(ctx, s.db, n.userID); err != nil || !confirmed {
		return t, false, err
	}
	it, err := s.readerFor(n.userID).item(ctx, n)
	if err != nil || it.Kind == KindChanged {
		return t, false, err
	}
	t = instantTarget{userID: n.userID, kind: n.kind}
	t.address, t.locale, err = s.recipientOf(ctx, s.db, n.userID, n.workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil // not a member any more
	}
	return t, err == nil, err
}

// readStored reads the reference and the time of a stored notification; a notification that can't
// be read is not mailed.
func readStored(ref string, n *stored) (created time.Time, ok bool) {
	if err := json.Unmarshal([]byte(ref), &n.ref); err != nil {
		return time.Time{}, false
	}
	created, err := time.Parse(timeFormat, n.createdAt)
	return created, err == nil
}

// runEmail makes one try of an instant mail: it checks that it may be mailed, reserves its place in
// the ledger (the hourly limit is counted there, and a mail that was submitted before is not
// submitted again), checks again at the last point that can still cancel it, and sends the kind's
// message with a link to the notification.
func (s *Service) runEmail(ctx context.Context, d emailDelivery) error {
	target, ok, err := s.instantEligible(ctx, d.notificationID)
	if err != nil {
		return s.retryEmail(ctx, d, err)
	}
	if !ok {
		return s.settleEmail(ctx, d, "failed")
	}
	key := "instant:" + strconv.FormatInt(d.notificationID, 10)
	got, _, err := s.reserve(ctx, target.userID, key, "instant", 0)
	if err != nil {
		return err
	}
	switch got {
	case slotFull:
		return s.settleEmail(ctx, d, "failed")
	case slotDone:
		// It was submitted by an earlier try, which may have died before it noted so.
		return s.settleEmail(ctx, d, "sent")
	}
	if s.afterReserve != nil {
		s.afterReserve()
	}
	target, ok, err = s.instantEligible(ctx, d.notificationID)
	if err != nil || !ok {
		state := ledgerCancelled
		if err != nil {
			state = ledgerRefused
		}
		err = errors.Join(err, s.settleLedger(ctx, key, state, 0))
		if err != nil {
			return s.retryEmail(ctx, d, err)
		}
		return s.settleEmail(ctx, d, "failed")
	}
	sendCtx, cancel := context.WithTimeout(ctx, emailSendTimeout)
	defer cancel()
	res := submitted(s.mail.SendTemplate(sendCtx, target.address, target.locale, "notification_"+target.kind, map[string]string{
		"action_url": s.publicURL + "/notification/" + strconv.FormatInt(d.notificationID, 10),
	}))
	if err := s.settleLedger(ctx, key, res.state, 0); err != nil {
		return err
	}
	switch {
	case res.state == ledgerSent:
		return s.settleEmail(ctx, d, "sent")
	case res.state == ledgerUnknown:
		slog.Warn("notifications: an email may or may not have been delivered; it isn't sent again", "delivery", d.id)
		return s.settleEmail(ctx, d, "sent")
	case res.state == ledgerCancelled || res.permanent:
		return s.settleEmail(ctx, d, "failed")
	}
	return s.retryEmail(ctx, d, res.cause)
}

// retryEmail moves a failed try's next one out, or gives the mail up after its last.
func (s *Service) retryEmail(ctx context.Context, d emailDelivery, cause error) error {
	if d.attempts >= emailMaxAttempts {
		return s.settleEmail(ctx, d, "failed")
	}
	after := min(emailBackoffBase<<(2*(d.attempts-1)), emailBackoffMax)
	slog.Warn("notifications: an email will be tried again", "delivery", d.id, "attempt", d.attempts, "err", cause)
	_, err := s.db.ExecContext(context.WithoutCancel(ctx), `UPDATE email_deliveries SET next_attempt_at = ?, updated_at = ? WHERE id = ?`, s.stamp(s.clock().Add(after)), s.now(), d.id)
	return err
}
