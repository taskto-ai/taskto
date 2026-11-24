package notifications

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"taskto.ai/taskto/internal/mail"
)

// Nothing is mailed to an address until its owner has proved it is theirs. A person who turns on
// any email choice (and has not done so for the address they have now) is sent one link; opening
// it, signed in as themselves, confirms the address, and until then every mail the choices would
// make is dropped (the inbox and push have it) and no summary is sent. The link:
//
//   - holds a random token of 256 bits that is stored only as its SHA-256, so the database can't
//     make a working link, and is never logged or put in an error;
//   - works once and expires after verifyTTL;
//   - names no one: it only leads to the app, which asks the signed-in person's own account to
//     confirm it (a link followed by someone else, or signed in as someone else, confirms nothing);
//   - is sent at most once every verifyInterval to a person, whatever they ask.
//
// What is confirmed is the address itself, as it is when the mail is sent: if the person's address
// changes, it is unconfirmed again, and a new link is sent when they next turn a choice on or ask
// for one.

// The bounds of a verification.
const (
	verifyTTL      = 24 * time.Hour
	verifyInterval = 5 * time.Minute
)

// The states of a person's address, as the settings say them.
const (
	// AddressVerified: mail may be sent to it.
	AddressVerified = "verified"
	// AddressPending: a link was sent and may still be opened.
	AddressPending = "pending"
	// AddressUnverified: no link is waiting.
	AddressUnverified = "unverified"
)

// EmailAddress is the person's address and whether notifications may be mailed to it.
type EmailAddress struct {
	Email string `json:"email"`
	State string `json:"state"`
}

// Errors of the verification.
var (
	// ErrVerifyInvalid is a token that is unknown, used, expired, or for an address the person no
	// longer has.
	ErrVerifyInvalid = errors.New("notifications: the confirmation link is unknown, used or expired")
	// ErrVerifyNotSent is a link that could not be mailed.
	ErrVerifyNotSent = errors.New("notifications: the confirmation email couldn't be sent")
)

// TooSoonError is a link asked for too soon after the last.
type TooSoonError struct{ Wait time.Duration }

func (e *TooSoonError) Error() string {
	return "notifications: a confirmation link was sent a moment ago"
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// addressOf says whether userID's address is confirmed, and its state for the settings.
func (s *Service) addressOf(ctx context.Context, q queryer, userID int64) (EmailAddress, error) {
	var a EmailAddress
	var verified, pending bool
	err := q.QueryRowContext(ctx, `
		SELECT u.email,
			coalesce(v.verified_at IS NOT NULL AND v.address = u.email, 0),
			coalesce(v.token_hash IS NOT NULL AND v.address = u.email AND v.expires_at > ?, 0)
		FROM users u LEFT JOIN notification_email_verification v ON v.user_id = u.id WHERE u.id = ?`,
		s.now(), userID).Scan(&a.Email, &verified, &pending)
	switch {
	case err != nil:
		return EmailAddress{}, err
	case verified:
		a.State = AddressVerified
	case pending:
		a.State = AddressPending
	default:
		a.State = AddressUnverified
	}
	return a, nil
}

// verified says whether mail may go to userID's address now.
func (s *Service) verified(ctx context.Context, q queryer, userID int64) (bool, error) {
	a, err := s.addressOf(ctx, q, userID)
	return a.State == AddressVerified, err
}

func hashVerifyToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SendVerification mails userID a link that confirms their address, unless it is confirmed already
// (then nothing is sent). workspaceID is where the language falls back to. It returns a
// *TooSoonError for a link sent within verifyInterval, ErrEmailUnavailable where the server can't
// send mail, and ErrVerifyNotSent when the mail didn't go (and then another may be asked for at once).
func (s *Service) SendVerification(ctx context.Context, userID, workspaceID int64) error {
	if !s.emailReady() {
		return ErrEmailUnavailable
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := hashVerifyToken(token)
	var address, locale string
	done := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		a, err := s.addressOf(ctx, tx, userID)
		if err != nil {
			return err
		}
		if a.State == AddressVerified {
			done = true
			return nil
		}
		var sentAt sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT sent_at FROM notification_email_verification WHERE user_id = ?`, userID).Scan(&sentAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if last, perr := time.Parse(timeFormat, sentAt.String); sentAt.Valid && perr == nil {
			if wait := verifyInterval - s.clock().Sub(last); wait > 0 {
				return &TooSoonError{Wait: wait}
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO notification_email_verification (user_id, address, token_hash, expires_at, sent_at, verified_at)
			VALUES (?, ?, ?, ?, ?, NULL)
			ON CONFLICT (user_id) DO UPDATE SET address = excluded.address, token_hash = excluded.token_hash,
				expires_at = excluded.expires_at, sent_at = excluded.sent_at, verified_at = NULL`,
			userID, a.Email, hash, s.stamp(s.clock().Add(verifyTTL)), s.now()); err != nil {
			return err
		}
		address, locale, err = s.recipientOf(ctx, tx, userID, workspaceID)
		return err
	})
	if err != nil || done {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, emailSendTimeout)
	defer cancel()
	err = s.mail.SendTemplate(sendCtx, address, locale, "verify_email", map[string]string{
		"action_url": s.publicURL + "/confirm-notification-email#" + token,
		"hours":      strconv.Itoa(int(verifyTTL / time.Hour)),
	})
	if err != nil {
		var se *mail.SendError
		if errors.As(err, &se) {
			slog.Warn("notifications: a confirmation email wasn't sent", "stage", se.Stage, "smtp_code", se.Code)
		} else {
			slog.Warn("notifications: a confirmation email couldn't be made", "err", err)
		}
		// The link was not sent: it is withdrawn, and another may be asked for now.
		_, _ = s.db.ExecContext(context.WithoutCancel(ctx), `
			UPDATE notification_email_verification SET token_hash = NULL, expires_at = NULL, sent_at = NULL
			WHERE user_id = ? AND token_hash = ?`, userID, hash)
		return ErrVerifyNotSent
	}
	return nil
}

// ConfirmAddress uses a link's token for userID: their address is confirmed, once.
func (s *Service) ConfirmAddress(ctx context.Context, userID int64, token string) error {
	if token == "" || len(token) > 200 {
		return ErrVerifyInvalid
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var hash, expires sql.NullString
		var current bool
		err := tx.QueryRowContext(ctx, `
			SELECT v.token_hash, v.expires_at, v.address = u.email
			FROM notification_email_verification v JOIN users u ON u.id = v.user_id WHERE v.user_id = ?`, userID).Scan(&hash, &expires, &current)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrVerifyInvalid
		}
		if err != nil {
			return err
		}
		if !hash.Valid || !current || !expires.Valid || expires.String <= s.now() ||
			subtle.ConstantTimeCompare([]byte(hash.String), []byte(hashVerifyToken(token))) != 1 {
			return ErrVerifyInvalid
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE notification_email_verification SET verified_at = ?, token_hash = NULL, expires_at = NULL WHERE user_id = ?`, s.now(), userID)
		return err
	})
}
