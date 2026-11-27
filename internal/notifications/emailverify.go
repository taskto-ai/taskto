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

const (
	verifyTTL      = 24 * time.Hour
	verifyInterval = 5 * time.Minute
)

const (
	AddressVerified   = "verified"
	AddressPending    = "pending"
	AddressUnverified = "unverified"
)

type EmailAddress struct {
	Email string `json:"email"`
	State string `json:"state"`
}

var (
	ErrVerifyInvalid = errors.New("notifications: the confirmation link is unknown, used or expired")
	ErrVerifyNotSent = errors.New("notifications: the confirmation email couldn't be sent")
)

type TooSoonError struct{ Wait time.Duration }

func (e *TooSoonError) Error() string {
	return "notifications: a confirmation link was sent a moment ago"
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

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

func (s *Service) verified(ctx context.Context, q queryer, userID int64) (bool, error) {
	a, err := s.addressOf(ctx, q, userID)
	return a.State == AddressVerified, err
}

func hashVerifyToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

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
		if !errors.As(err, &se) {
			se = nil
		}
		if se != nil {
			slog.Warn("notifications: a confirmation email wasn't sent", "stage", se.Stage, "smtp_code", se.Code)
		} else {
			slog.Warn("notifications: a confirmation email couldn't be made", "err", err)
		}
		if se != nil && se.Ambiguous {
			return nil
		}
		for attempt := 0; attempt < 2; attempt++ {
			_, werr := s.db.ExecContext(context.WithoutCancel(ctx), `
				UPDATE notification_email_verification SET token_hash = NULL, expires_at = NULL, sent_at = NULL
				WHERE user_id = ? AND token_hash = ?`, userID, hash)
			if werr == nil {
				break
			}
			slog.Warn("notifications: a confirmation link that wasn't sent couldn't be withdrawn", "attempt", attempt+1, "err", werr)
		}
		return ErrVerifyNotSent
	}
	return nil
}

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
