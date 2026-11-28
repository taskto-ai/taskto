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

	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/mail"
	"taskto.ai/taskto/internal/schedule"
)

const (
	ModeOff     = "off"
	ModeInstant = "instant"
	ModeDigest  = "digest"
)

const KindDigest schedule.Kind = "notifications.digest"

const (
	DefaultDigestHour = 8
	DefaultTimezone   = "UTC"
	emailTTL          = 24 * time.Hour
	digestWindow      = 48 * time.Hour
	maxInstantPerHour = 20
	emailMaxAttempts  = 5
	emailSendTimeout  = 30 * time.Second
	emailBackoffBase  = time.Minute
	emailBackoffMax   = time.Hour
	digestTries       = 3
)

func EmailKinds() []string {
	return []string{KindCardAssigned, KindCardComment, KindCardMention, KindCardDue, KindCardOverdue, KindAppEffectFailed, KindAppReminder}
}

func emailKind(kind string) bool {
	for _, k := range EmailKinds() {
		if k == kind {
			return true
		}
	}
	return false
}

type EmailChoices struct {
	Available bool              `json:"available"`
	Address   EmailAddress      `json:"address"`
	Kinds     map[string]string `json:"kinds"`
	Digest    EmailDigest       `json:"digest"`
}

type EmailDigest struct {
	Hour     int    `json:"hour"`
	Timezone string `json:"timezone"`
	Chosen   bool   `json:"chosen"`
}

type EmailUpdate struct {
	Kinds  map[string]string `json:"kinds"`
	Digest *EmailDigest      `json:"digest"`
}

var ErrEmailInvalid = errors.New("notifications: invalid email choice")

var ErrNotMember = errors.New("notifications: not a member of the workspace")

var ErrEmailUnavailable = errors.New("notifications: the server can't send email")

func (s *Service) emailReady() bool {
	return s.mail.Configured() && s.publicURL != ""
}

func (s *Service) AttachScheduler(sch *schedule.Service) error {
	if err := sch.Register(KindDigest, s.sendDigest); err != nil {
		return err
	}
	s.sched = sch
	return nil
}

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
	err = s.db.QueryRowContext(ctx, digestSettingsSQL, userID, workspaceID).Scan(&out.Digest.Hour, &out.Digest.Timezone, &out.Digest.Chosen)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
		if zone, zerr := auth.UserTimeZone(ctx, s.db, userID); zerr != nil {
			err = zerr
		} else if zone != "" {
			out.Digest.Timezone = zone
		}
	}
	if err != nil {
		return EmailChoices{}, err
	}
	return out, nil
}

const digestSettingsSQL = `
	SELECT s.digest_hour,
	       CASE WHEN s.timezone_chosen = 0 AND u.time_zone <> '' THEN u.time_zone ELSE s.timezone END,
	       s.timezone_chosen
	FROM notification_email_settings s JOIN users u ON u.id = s.user_id
	WHERE s.user_id = ? AND s.workspace_id = ?`

func (s *Service) SyncTimeZone(ctx context.Context, tx *sql.Tx, userID int64) error {
	if s.sched == nil {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT workspace_id FROM notification_email_settings WHERE user_id = ? AND timezone_chosen = 0 ORDER BY workspace_id`, userID)
	if err != nil {
		return err
	}
	var workspaces []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		workspaces = append(workspaces, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range workspaces {
		if uses, err := s.usesDigest(ctx, tx, userID, id); err != nil {
			return err
		} else if !uses {
			continue
		}
		if err := s.scheduleDigest(ctx, tx, userID, id); err != nil {
			return err
		}
	}
	return nil
}

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
		var prevChosen bool
		err := tx.QueryRowContext(ctx, digestSettingsSQL, userID, workspaceID).Scan(&prevHour, &prevZone, &prevChosen)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		known := err == nil
		for kind, mode := range u.Kinds {
			var err error
			if mode == ModeOff {
				_, err = tx.ExecContext(ctx, `DELETE FROM notification_email_prefs WHERE user_id = ? AND workspace_id = ? AND kind = ?`, userID, workspaceID, kind)
			} else {
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

func (s *Service) scheduleDigest(ctx context.Context, q schedule.DBTX, userID, workspaceID int64) error {
	var hour int
	var zone string
	var chosen bool
	if err := q.QueryRowContext(ctx, digestSettingsSQL, userID, workspaceID).Scan(&hour, &zone, &chosen); err != nil {
		return err
	}
	loc, err := loadZone(zone)
	if err != nil {
		if chosen {
			return err
		}
		slog.WarnContext(ctx, "notifications: a time zone is unknown here, using UTC for a summary", "user", userID, "zone", zone)
		loc = time.UTC
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

func nextDigest(now time.Time, hour int, loc *time.Location) time.Time {
	local := now.In(loc)
	at := schedule.LocalHour(local.Year(), local.Month(), local.Day(), hour, loc)
	if !at.After(now) {
		at = schedule.LocalHour(local.Year(), local.Month(), local.Day()+1, hour, loc)
	}
	return at.UTC()
}

func (s *Service) usesDigest(ctx context.Context, q queryer, userID, workspaceID int64) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM notification_email_prefs WHERE user_id = ? AND workspace_id = ? AND mode = 'digest'`, userID, workspaceID).Scan(&n)
	return n > 0, err
}

func (s *Service) recipientOf(ctx context.Context, q queryer, userID, workspaceID int64) (address, locale string, err error) {
	return s.recipient(ctx, q, userID, workspaceID, false)
}

func (s *Service) confirmedRecipientOf(ctx context.Context, q queryer, userID, workspaceID int64) (address, locale string, err error) {
	return s.recipient(ctx, q, userID, workspaceID, true)
}

func (s *Service) recipient(ctx context.Context, q queryer, userID, workspaceID int64, confirmed bool) (address, locale string, err error) {
	var userLocale, workspaceLocale string
	err = q.QueryRowContext(ctx, `
		SELECT u.email, u.locale, w.locale FROM users u, workspaces w
		WHERE u.id = ? AND w.id = ? AND EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = u.id AND m.workspace_id = w.id)
		  AND (NOT ? OR EXISTS (SELECT 1 FROM notification_email_verification v
		                        WHERE v.user_id = u.id AND v.verified_at IS NOT NULL AND v.address = u.email))`,
		userID, workspaceID, confirmed).Scan(&address, &userLocale, &workspaceLocale)
	if userLocale != "" {
		return address, userLocale, err
	}
	return address, workspaceLocale, err
}

const (
	ledgerSending   = "sending"
	ledgerSent      = "sent"
	ledgerRefused   = "refused"
	ledgerUnknown   = "unknown"
	ledgerCancelled = "cancelled"
)

type slot int

const (
	slotReserved slot = iota
	slotDone
	slotFull
)

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

func (s *Service) settleLedger(ctx context.Context, key, state string, upto int64) error {
	_, err := s.db.ExecContext(context.WithoutCancel(ctx), `
		UPDATE notification_email_ledger SET state = ?, upto = ?, updated_at = ? WHERE key = ? AND state = 'sending'`, state, upto, s.now(), key)
	return err
}

type submission struct {
	state     string
	permanent bool
	cause     error
}

func submitted(err error) submission {
	var se *mail.SendError
	switch {
	case err == nil:
		return submission{state: ledgerSent}
	case errors.Is(err, mail.ErrNotConfigured):
		return submission{state: ledgerCancelled}
	case errors.As(err, &se):
		slog.Warn("notifications: an email wasn't sent", "stage", se.Stage, "smtp_code", se.Code, "ambiguous", se.Ambiguous)
		cause := fmt.Errorf("mail: %s: %d", se.Stage, se.Code)
		if se.Ambiguous {
			return submission{state: ledgerUnknown, permanent: true, cause: cause}
		}
		return submission{state: ledgerRefused, permanent: se.Stage == mail.StageRejected && se.Code >= 500, cause: cause}
	}
	slog.Warn("notifications: an email couldn't be made", "err", err)
	return submission{state: ledgerRefused, cause: err}
}

type digestPlan struct {
	count           int
	upto            int64
	address, locale string
}

func (s *Service) digestPlanFor(ctx context.Context, job schedule.Job) (p digestPlan, ok bool, err error) {
	err = s.write(ctx, func(tx *sql.Tx) error {
		p, ok, err = s.digestPlanIn(ctx, tx, job)
		return err
	})
	return p, ok && err == nil, err
}

func (s *Service) digestPlanIn(ctx context.Context, tx *sql.Tx, job schedule.Job) (p digestPlan, ok bool, err error) {
	if !s.emailReady() {
		return p, false, nil
	}
	if confirmed, err := s.verified(ctx, tx, job.UserID); err != nil || !confirmed {
		return p, false, err
	}
	var cursor int64
	err = tx.QueryRowContext(ctx, `SELECT digest_cursor FROM notification_email_settings WHERE user_id = ? AND workspace_id = ?`,
		job.UserID, job.WorkspaceID).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	rows, err := tx.QueryContext(ctx, `
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
	reader := s.readerOn(s.access.On(tx), job.UserID)
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
		return p, false, advanceDigestIn(ctx, tx, job, p.upto)
	}
	p.address, p.locale, err = s.confirmedRecipientOf(ctx, tx, job.UserID, job.WorkspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (s *Service) advanceDigest(ctx context.Context, job schedule.Job, upto int64) error {
	return advanceDigestIn(context.WithoutCancel(ctx), s.db, job, upto)
}

func advanceDigestIn(ctx context.Context, x interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, job schedule.Job, upto int64) error {
	_, err := x.ExecContext(ctx, `
		UPDATE notification_email_settings SET digest_cursor = ? WHERE user_id = ? AND workspace_id = ? AND digest_cursor < ?`,
		upto, job.UserID, job.WorkspaceID, upto)
	return err
}

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

func (s *Service) sendDigest(ctx context.Context, job schedule.Job) error {
	err := s.digestOnce(ctx, job)
	if err != nil && ctx.Err() == nil && job.Attempts < digestTries {
		return err
	}
	if err != nil {
		slog.Warn("notifications: a summary was left for the next one", "job", job.ID, "err", err)
	}
	return s.digestNext(ctx, job)
}

func (s *Service) digestNext(ctx context.Context, job schedule.Job) error {
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
		return s.advanceDigest(ctx, job, covered)
	}
	if s.afterReserve != nil {
		s.afterReserve()
	}
	if cur, err := s.current(ctx, s.db, job); err != nil || !cur {
		return errors.Join(err, s.settleLedger(ctx, key, ledgerCancelled, plan.upto))
	}
	cancelled := false
	err = s.write(ctx, func(tx *sql.Tx) error {
		if cur, err := s.current(ctx, tx, job); err != nil || !cur {
			cancelled = true
			return err
		}
		var err error
		plan, ok, err = s.digestPlanIn(ctx, tx, job)
		if err != nil || !ok {
			cancelled = true
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE notification_email_ledger SET upto = ?, updated_at = ? WHERE key = ? AND state = 'sending'`,
			plan.upto, s.now(), key)
		return err
	})
	if err != nil || cancelled {
		state := ledgerCancelled
		if err != nil {
			state = ledgerRefused
		}
		return errors.Join(err, s.settleLedger(ctx, key, state, plan.upto))
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

type emailDelivery struct {
	id, notificationID int64
	attempts           int
}

func (s *Service) DrainEmailOnce(ctx context.Context) { s.drainEmail(ctx) }

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

type instantTarget struct {
	userID          int64
	kind            string
	address, locale string
}

func (s *Service) instantEligible(ctx context.Context, id int64) (t instantTarget, ok bool, err error) {
	err = s.write(ctx, func(tx *sql.Tx) error {
		t, ok, err = s.instantEligibleIn(ctx, tx, id)
		return err
	})
	if err != nil {
		return instantTarget{}, false, err
	}
	return t, ok, nil
}

func (s *Service) instantEligibleIn(ctx context.Context, tx *sql.Tx, id int64) (t instantTarget, ok bool, err error) {
	var n stored
	var ref string
	var mode sql.NullString
	var member bool
	err = tx.QueryRowContext(ctx, `
		SELECT n.id, n.user_id, n.workspace_id, n.kind, n.ref, n.created_at, n.read_at IS NOT NULL,
			(SELECT mode FROM notification_email_prefs p WHERE p.user_id = n.user_id AND p.workspace_id = n.workspace_id AND p.kind = n.kind),
			EXISTS (SELECT 1 FROM memberships m WHERE m.workspace_id = n.workspace_id AND m.user_id = n.user_id)
		FROM notifications n WHERE n.id = ?`, id).Scan(&n.id, &n.userID, &n.workspaceID, &n.kind, &ref, &n.createdAt, &n.read, &mode, &member)
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	created, readable := readStored(ref, &n)
	if !readable || n.read || !member || mode.String != ModeInstant || s.clock().Sub(created) > emailTTL || !emailKind(n.kind) || !s.emailReady() {
		return t, false, nil
	}
	t = instantTarget{userID: n.userID, kind: n.kind}
	t.address, t.locale, err = s.confirmedRecipientOf(ctx, tx, n.userID, n.workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return instantTarget{}, false, nil
	}
	if err != nil {
		return instantTarget{}, false, err
	}
	if s.betweenReads != nil {
		s.betweenReads(tx)
	}
	it, err := s.readerOn(s.access.On(tx), n.userID).item(ctx, n)
	if err != nil || it.Kind == KindChanged {
		return instantTarget{}, false, err
	}
	return t, true, nil
}

func readStored(ref string, n *stored) (created time.Time, ok bool) {
	if err := json.Unmarshal([]byte(ref), &n.ref); err != nil {
		return time.Time{}, false
	}
	created, err := time.Parse(timeFormat, n.createdAt)
	return created, err == nil
}

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

func (s *Service) retryEmail(ctx context.Context, d emailDelivery, cause error) error {
	if d.attempts >= emailMaxAttempts {
		return s.settleEmail(ctx, d, "failed")
	}
	after := min(emailBackoffBase<<(2*(d.attempts-1)), emailBackoffMax)
	slog.Warn("notifications: an email will be tried again", "delivery", d.id, "attempt", d.attempts, "err", cause)
	_, err := s.db.ExecContext(context.WithoutCancel(ctx), `UPDATE email_deliveries SET next_attempt_at = ?, updated_at = ? WHERE id = ?`, s.stamp(s.clock().Add(after)), s.now(), d.id)
	return err
}
