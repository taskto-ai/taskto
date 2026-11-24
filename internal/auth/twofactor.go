package auth

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

// Two-factor login (docs/api.md#two-factor-login).
//
//   - A user turns it on in their own settings: with their password the server makes a
//     secret, sealed with the server key, and shows it (as an otpauth URI the web app draws
//     as a QR code, and as text). It asks for a code from the authenticator app before it
//     turns anything on, and then shows ten recovery codes, once.
//   - A password login of a user with it on doesn't make a session: it makes a challenge, a
//     short-lived token that is not a session and that no route but the second step reads.
//     The second step takes the challenge and a code, from the app (RFC 6238, a step either
//     side, each step's code once) or a recovery code (once), and only then makes the session.
//   - Turning it off and making new recovery codes take the password and a code. An admin
//     can clear a member's, under the rule for making them a password reset link.
//   - Signing in with a provider (sso.go) has no second step: the provider's own sign-in
//     is the second factor, or not, as its operator decided. Access tokens never had one.

var (
	errWrongCode        = errors.New("the code is wrong or was already used")
	errChallengeInvalid = errors.New("the sign-in expired or was used: enter your password again")
	errTwoFactorOn      = errors.New("two-factor login is already on")
	errTwoFactorOff     = errors.New("two-factor login is not on")
	errNotStarted       = errors.New("two-factor login was not started: ask for a new secret")
	errNoServerKey      = errors.New("the server has no key to seal two-factor secrets with")
	errClearOwn         = errors.New("turn off your own two-factor login in Settings, with your password and a code")
	errClearElsewhere   = errors.New("this member also belongs to a workspace you aren't an admin of: only someone who administers all of their workspaces can clear their two-factor login")
)

const (
	// challengeTTL is how long someone has to give the second step after the password.
	challengeTTL = 5 * time.Minute
	// challengeAttempts is how many wrong codes one challenge takes before it is dropped.
	challengeAttempts = 5
	// enrolmentTTL is how long the secret an enrolment showed can be confirmed.
	enrolmentTTL = 15 * time.Minute
)

// The second step's limits, in wrong codes per window. They have the login's shape, with a
// lower ceiling per account: a six-digit code has fewer possibilities than a password.
const (
	codeIPAttempts           = 30
	codeAccountAndIPAttempts = 10
	codeAccountAttempts      = 30
)

func (s *Service) twoFactorRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/login/two-factor", refuseTokens(s.handleLoginTwoFactor))
	own := func(h http.HandlerFunc) http.Handler { return refuseTokens(s.RequireUser(h).ServeHTTP) }
	mux.Handle("GET /api/me/two-factor", own(s.handleTwoFactorStatus))
	mux.Handle("POST /api/me/two-factor/enrol", own(s.handleTwoFactorEnrol))
	mux.Handle("POST /api/me/two-factor/confirm", own(s.handleTwoFactorConfirm))
	mux.Handle("POST /api/me/two-factor/disable", own(s.handleTwoFactorDisable))
	mux.Handle("POST /api/me/two-factor/recovery-codes", own(s.handleTwoFactorRecoveryCodes))
	mux.Handle("DELETE /api/workspaces/{workspace}/members/{user}/two-factor",
		refuseTokens(s.RequireAdmin(http.HandlerFunc(s.handleClearTwoFactor)).ServeHTTP))
}

// limitKey is a limiter and the key one attempt counts against.
type limitKey struct {
	l   *limiter
	key string
}

// takeAll counts an attempt against every limit, or against none if one is used up.
func takeAll(ls []limitKey) (bool, time.Duration) {
	for i, lim := range ls {
		if ok, wait := lim.l.take(lim.key); !ok {
			giveAll(ls[:i])
			return false, wait
		}
	}
	return true, 0
}

func giveAll(ls []limitKey) {
	for _, lim := range ls {
		lim.l.give(lim.key)
	}
}

func tooManyCodes(w http.ResponseWriter, wait time.Duration) {
	limited(w, wait, apierr.CodeTooManyTwoFactorCodes, "too many wrong codes: try again later")
}

// accountLimits are the second step's limits for one user from one address. The key is the
// user's ID, which the caller only has with a challenge or a session of that user.
func (s *Service) accountLimits(ip string, userID int64) []limitKey {
	id := strconv.FormatInt(userID, 10)
	return []limitKey{{s.codePerAccountIP, id + "|" + ip}, {s.codePerAccount, id}}
}

func (s *Service) sealAD(userID int64) []byte {
	return []byte("taskto auth totp v1 user " + strconv.FormatInt(userID, 10))
}

// recoveryHash is what is stored for a (normalized) recovery code: a keyed hash, so a copy of
// the database alone doesn't let anyone check guesses.
func (s *Service) recoveryHash(userID int64, code string) string {
	return hex.EncodeToString(s.key.MAC("auth recovery code v1", []byte(strconv.FormatInt(userID, 10)+":"+code)))
}

// twoFactorOn reports whether userID has two-factor login on.
func (s *Service) twoFactorOn(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, userID int64) (bool, error) {
	var on bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM user_totp WHERE user_id = ? AND enabled_at IS NOT NULL)`, userID).Scan(&on)
	return on, err
}

// useCode spends a code of userID's within tx: a TOTP code of a step later than the last one
// used, or, for a user with two-factor login on, an unused recovery code. With pending, it
// takes the TOTP code of a secret that isn't confirmed yet, and no recovery code. It
// fails with errWrongCode for anything else.
func (s *Service) useCode(ctx context.Context, tx *sql.Tx, userID int64, raw string, pending bool) error {
	if s.key == nil {
		return errNoServerKey
	}
	code := normalizeCode(raw)
	switch {
	case isTOTPCode(code):
		var sealed []byte
		var last int64
		err := tx.QueryRowContext(ctx, `SELECT secret, last_step FROM user_totp WHERE user_id = ? AND (enabled_at IS NULL) = ?`,
			userID, pending).Scan(&sealed, &last)
		if errors.Is(err, sql.ErrNoRows) {
			return errWrongCode
		}
		if err != nil {
			return err
		}
		secret, err := s.key.Open(sealed, s.sealAD(userID))
		if err != nil {
			return fmt.Errorf("opening a two-factor secret: %w", err)
		}
		step, ok := matchTOTP(secret, code, s.now().Unix(), last)
		if !ok {
			return errWrongCode
		}
		// The step is taken in the same statement that checks it is still later than the last
		// one, so two requests with the same code can't both pass.
		res, err := tx.ExecContext(ctx, `UPDATE user_totp SET last_step = ? WHERE user_id = ? AND last_step < ?`, step, userID, step)
		if err != nil {
			return err
		}
		return mustAffect(res, errWrongCode)
	case !pending && len(code) == recoveryLen:
		res, err := tx.ExecContext(ctx, `
			UPDATE user_recovery_codes SET used_at = ?
			WHERE user_id = ? AND code_hash = ? AND used_at IS NULL
			  AND EXISTS (SELECT 1 FROM user_totp WHERE user_id = ? AND enabled_at IS NOT NULL)`,
			stamp(s.now()), userID, s.recoveryHash(userID, code), userID)
		if err != nil {
			return err
		}
		return mustAffect(res, errWrongCode)
	}
	return errWrongCode
}

// replaceRecoveryCodes drops userID's recovery codes and makes new ones, returned in clear
// this once.
func (s *Service) replaceRecoveryCodes(ctx context.Context, tx *sql.Tx, userID int64) ([]string, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_recovery_codes WHERE user_id = ?`, userID); err != nil {
		return nil, err
	}
	codes := make([]string, 0, recoveryCodeCount)
	for range recoveryCodeCount {
		code := newRecoveryCode()
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_recovery_codes (user_id, code_hash, created_at) VALUES (?, ?, ?)`,
			userID, s.recoveryHash(userID, normalizeCode(code)), stamp(s.now())); err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, nil
}

// checkOwnPassword checks the password of the caller u, counting a wrong one as a failed
// login: a session someone else got hold of is no faster a way to guess it than the login
// form. It answers, and returns false, unless the password is right.
func (s *Service) checkOwnPassword(w http.ResponseWriter, r *http.Request, u User, password string) bool {
	if password == "" {
		writeError(w, http.StatusBadRequest, apierr.CodeCurrentPasswordRequired, "the current password is required")
		return false
	}
	_, hash, err := s.userByEmail(r.Context(), u.Email)
	if err != nil {
		serverError(w, r, err)
		return false
	}
	ip := s.clientIP(r)
	limits := []limitKey{{s.perIP, ip}, {s.perAccountIP, u.Email + "|" + ip}, {s.perAccount, u.Email}}
	if ok, wait := takeAll(limits); !ok {
		limited(w, wait, apierr.CodeTooManyPasswords, "too many wrong passwords: try again later")
		return false
	}
	// An account made by signing in with a provider has no password, and nothing to protect
	// with a second factor at the password login.
	if hash == "" {
		giveAll(limits)
		writeError(w, http.StatusForbidden, apierr.CodeTwoFactorNoPassword, "this account has no password to protect with a second factor")
		return false
	}
	match, err := s.hasher.verify(r.Context(), password, hash)
	if match || err != nil {
		giveAll(limits)
	}
	if err != nil {
		serverError(w, r, err)
		return false
	}
	if !match {
		// 403, not 401: the session is fine, and 401 would send the web app to the login.
		writeError(w, http.StatusForbidden, apierr.CodeWrongPassword, errWrongPassword.Error())
		return false
	}
	return true
}

// failTwoFactor answers for the errors of the routes of one's own two-factor login.
func failTwoFactor(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errWrongCode):
		// 403, not 401, as with a wrong password: the session is fine.
		writeError(w, http.StatusForbidden, apierr.CodeWrongTwoFactorCode, err.Error())
	case errors.Is(err, errTwoFactorOn):
		writeError(w, http.StatusConflict, apierr.CodeTwoFactorAlreadyOn, err.Error())
	case errors.Is(err, errTwoFactorOff):
		writeError(w, http.StatusConflict, apierr.CodeTwoFactorNotOn, err.Error())
	case errors.Is(err, errNotStarted):
		writeError(w, http.StatusConflict, apierr.CodeTwoFactorNotStarted, err.Error())
	default:
		serverError(w, r, err)
	}
}

func (s *Service) needKey(w http.ResponseWriter, r *http.Request) bool {
	if s.key == nil {
		serverError(w, r, errNoServerKey)
		return false
	}
	return true
}

func (s *Service) handleTwoFactorStatus(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	on, err := s.twoFactorOn(r.Context(), s.db, u.ID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	left := 0
	if on {
		if err := s.db.QueryRowContext(r.Context(), `SELECT count(*) FROM user_recovery_codes WHERE user_id = ? AND used_at IS NULL`, u.ID).Scan(&left); err != nil {
			serverError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": on, "recovery_codes_left": left})
}

// handleTwoFactorEnrol makes a secret for the caller, not yet on: confirming it with a code
// turns two-factor login on. Asking again before that makes a new secret.
func (s *Service) handleTwoFactorEnrol(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) || !s.needKey(w, r) {
		return
	}
	// Two-factor login protects the password login: with passwords off it protects nothing.
	if s.opts.DisablePasswordLogin {
		passwordLoginDisabled(w)
		return
	}
	if !s.checkOwnPassword(w, r, u, req.Password) {
		return
	}
	secret := newTOTPSecret()
	sealed, err := s.key.Seal(secret, s.sealAD(u.ID))
	if err != nil {
		serverError(w, r, err)
		return
	}
	now := s.now()
	enrolment, enrolmentHash := newToken()
	err = s.write(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO user_totp (user_id, secret, enrolment_id, enrolment_expires_at, created_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (user_id) DO UPDATE SET secret = excluded.secret, last_step = 0,
				enrolment_id = excluded.enrolment_id, enrolment_expires_at = excluded.enrolment_expires_at,
				created_at = excluded.created_at
			WHERE user_totp.enabled_at IS NULL`,
			u.ID, sealed, enrolmentHash, stamp(now.Add(enrolmentTTL)), stamp(now))
		if err != nil {
			return err
		}
		return mustAffect(res, errTwoFactorOn)
	})
	if err != nil {
		failTwoFactor(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"enrolment":   enrolment,
		"secret":      secretEncoding.EncodeToString(secret),
		"otpauth_uri": totpURI(secret, u.Email),
	})
}

// handleTwoFactorConfirm turns two-factor login on, for a code of the secret handleTwoFactorEnrol
// made, and shows the recovery codes.
func (s *Service) handleTwoFactorConfirm(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	var req struct {
		Enrolment string `json:"enrolment"`
		Code      string `json:"code"`
	}
	if !readJSON(w, r, &req) || !s.needKey(w, r) {
		return
	}
	limits := append([]limitKey{{s.codePerIP, s.clientIP(r)}}, s.accountLimits(s.clientIP(r), u.ID)...)
	if ok, wait := takeAll(limits); !ok {
		tooManyCodes(w, wait)
		return
	}
	var codes []string
	err := s.write(r.Context(), func(tx *sql.Tx) error {
		var pending bool
		// The code must be for the enrolment that showed the secret: one that was replaced by a
		// later enrolment (another tab's), or that ran out, is not confirmable.
		if err := tx.QueryRowContext(r.Context(), `
			SELECT EXISTS (SELECT 1 FROM user_totp
				WHERE user_id = ? AND enabled_at IS NULL AND enrolment_id = ? AND enrolment_expires_at > ?)`,
			u.ID, hashToken(req.Enrolment), stamp(s.now())).Scan(&pending); err != nil {
			return err
		}
		if !pending {
			if on, err := s.twoFactorOn(r.Context(), tx, u.ID); err != nil {
				return err
			} else if on {
				return errTwoFactorOn
			}
			return errNotStarted
		}
		if err := s.useCode(r.Context(), tx, u.ID, req.Code, true); err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE user_totp SET enabled_at = ?, enrolment_id = NULL, enrolment_expires_at = NULL WHERE user_id = ?`,
			stamp(s.now()), u.ID); err != nil {
			return err
		}
		var err error
		codes, err = s.replaceRecoveryCodes(r.Context(), tx, u.ID)
		return err
	})
	if err != nil {
		if !errors.Is(err, errWrongCode) {
			giveAll(limits)
		}
		failTwoFactor(w, r, err)
		return
	}
	giveAll(limits)
	slog.InfoContext(r.Context(), "two-factor login turned on", "user_id", u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// ownFactorAction runs act, within a transaction that first spends the caller's code (a TOTP
// or a recovery code), after checking their password. It answers any failure and reports
// whether act ran.
func (s *Service) ownFactorAction(w http.ResponseWriter, r *http.Request, password, code string, act func(tx *sql.Tx, userID int64) error) bool {
	u, _ := UserFrom(r.Context())
	if !s.needKey(w, r) || !s.checkOwnPassword(w, r, u, password) {
		return false
	}
	limits := append([]limitKey{{s.codePerIP, s.clientIP(r)}}, s.accountLimits(s.clientIP(r), u.ID)...)
	if ok, wait := takeAll(limits); !ok {
		tooManyCodes(w, wait)
		return false
	}
	err := s.write(r.Context(), func(tx *sql.Tx) error {
		if on, err := s.twoFactorOn(r.Context(), tx, u.ID); err != nil {
			return err
		} else if !on {
			return errTwoFactorOff
		}
		if err := s.useCode(r.Context(), tx, u.ID, code, false); err != nil {
			return err
		}
		return act(tx, u.ID)
	})
	if err != nil {
		if !errors.Is(err, errWrongCode) {
			giveAll(limits)
		}
		failTwoFactor(w, r, err)
		return false
	}
	giveAll(limits)
	return true
}

func (s *Service) handleTwoFactorDisable(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ok := s.ownFactorAction(w, r, req.Password, req.Code, func(tx *sql.Tx, userID int64) error {
		return dropTwoFactor(r.Context(), tx, userID)
	})
	if !ok {
		return
	}
	slog.InfoContext(r.Context(), "two-factor login turned off", "user_id", u.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleTwoFactorRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var codes []string
	ok := s.ownFactorAction(w, r, req.Password, req.Code, func(tx *sql.Tx, userID int64) error {
		var err error
		codes, err = s.replaceRecoveryCodes(r.Context(), tx, userID)
		return err
	})
	if !ok {
		return
	}
	slog.InfoContext(r.Context(), "recovery codes made again", "user_id", u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// dropTwoFactor removes userID's secret and recovery codes, and the half-done logins that
// were waiting on them.
func dropTwoFactor(ctx context.Context, tx *sql.Tx, userID int64) error {
	for _, q := range []string{
		`DELETE FROM user_totp WHERE user_id = ?`,
		`DELETE FROM user_recovery_codes WHERE user_id = ?`,
		`DELETE FROM login_challenges WHERE user_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, userID); err != nil {
			return err
		}
	}
	return nil
}

// signIn is what a verified password earns: a session, or for a user with two-factor login
// on a challenge for the second step and no session.
type signIn struct {
	session   string
	challenge string
	expires   string
}

// issueSignIn makes what userID's verified password earns, within tx: the factor state is
// read here, in the transaction that inserts the session or the challenge, so enrolling
// that commits meanwhile can't leave a password-only session behind.
func (s *Service) issueSignIn(ctx context.Context, tx *sql.Tx, userID int64) (signIn, error) {
	on, err := s.twoFactorOn(ctx, tx, userID)
	if err != nil {
		return signIn{}, err
	}
	if !on {
		session, err := s.insertSession(ctx, tx, userID)
		return signIn{session: session}, err
	}
	now := s.now()
	out := signIn{expires: stamp(now.Add(challengeTTL))}
	var hash string
	out.challenge, hash = newToken()
	if _, err := tx.ExecContext(ctx, `DELETE FROM login_challenges WHERE expires_at <= ?`, stamp(now)); err != nil {
		return signIn{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO login_challenges (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hash, userID, stamp(now), out.expires)
	return out, err
}

// passwordSignIn is issueSignIn for a login whose password was checked against verifiedHash:
// it fails with errPasswordChanged if the user's password isn't that hash any more (changed
// or reset since), which also ended the challenges the old one could have made.
func (s *Service) passwordSignIn(ctx context.Context, userID int64, verifiedHash string) (signIn, error) {
	var out signIn
	err := s.write(ctx, func(tx *sql.Tx) error {
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&current); err != nil {
			return err
		}
		if current != verifiedHash {
			return errPasswordChanged
		}
		var err error
		out, err = s.issueSignIn(ctx, tx, userID)
		return err
	})
	return out, err
}

// writeChallenge answers a password step that needs a code.
func writeChallenge(w http.ResponseWriter, out signIn) {
	writeJSON(w, http.StatusOK, map[string]any{"two_factor_required": true, "challenge": out.challenge, "expires_at": out.expires})
}

// handleLoginTwoFactor is the second step of a password login that asked for a code: it
// makes the session, once, for a challenge and a right code.
func (s *Service) handleLoginTwoFactor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Challenge string `json:"challenge"`
		Code      string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Code == "" || len(req.Code) > 64 {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "a code is required")
		return
	}
	// The address's limit first, so a client adds at most codeIPAttempts keys to the other
	// two per window, whatever challenges it tries. A challenge that doesn't exist counts.
	ip := s.clientIP(r)
	perIP := []limitKey{{s.codePerIP, ip}}
	if ok, wait := takeAll(perIP); !ok {
		tooManyCodes(w, wait)
		return
	}
	fail := func() {
		writeError(w, http.StatusUnauthorized, apierr.CodeTwoFactorChallenge, errChallengeInvalid.Error())
	}
	if req.Challenge == "" || len(req.Challenge) > 128 {
		fail()
		return
	}
	// Only to know whose limits to count against; the transaction below looks the challenge up
	// again, by its hash and this user, and is what decides.
	challengeHash := hashToken(req.Challenge)
	var userID int64
	err := s.db.QueryRowContext(r.Context(), `SELECT user_id FROM login_challenges WHERE token_hash = ? AND expires_at > ?`,
		challengeHash, stamp(s.now())).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		fail()
		return
	}
	if err != nil {
		giveAll(perIP)
		serverError(w, r, err)
		return
	}
	account := s.accountLimits(ip, userID)
	if ok, wait := takeAll(account); !ok {
		tooManyCodes(w, wait)
		return
	}
	s.at("challenge-found")
	var token string
	var wrong bool
	err = s.write(r.Context(), func(tx *sql.Tx) error {
		// The challenge is read, its attempts checked and counted, the code spent and the
		// challenge used up in this one transaction, which holds the write lock: requests
		// racing on one challenge go one at a time, and can't take it past its attempts. It is
		// always addressed by its hash and its user, never by a row id that could be reused.
		var attempts int
		err := tx.QueryRowContext(r.Context(), `
			SELECT attempts FROM login_challenges WHERE token_hash = ? AND user_id = ? AND expires_at > ?`,
			challengeHash, userID, stamp(s.now())).Scan(&attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return errChallengeInvalid
		}
		if err != nil {
			return err
		}
		if attempts >= challengeAttempts {
			return errChallengeInvalid
		}
		err = s.useCode(r.Context(), tx, userID, req.Code, false)
		if errors.Is(err, errWrongCode) {
			// The wrong code is counted, and commits. Too many wrong codes for one challenge ends
			// it: the password goes in again.
			wrong = true
			if _, err := tx.ExecContext(r.Context(), `
				UPDATE login_challenges SET attempts = attempts + 1 WHERE token_hash = ? AND user_id = ?`,
				challengeHash, userID); err != nil {
				return err
			}
			_, err := tx.ExecContext(r.Context(), `
				DELETE FROM login_challenges WHERE token_hash = ? AND user_id = ? AND attempts >= ?`,
				challengeHash, userID, challengeAttempts)
			return err
		}
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(r.Context(), `DELETE FROM login_challenges WHERE token_hash = ? AND user_id = ? AND expires_at > ?`,
			challengeHash, userID, stamp(s.now()))
		if err != nil {
			return err
		}
		if err := mustAffect(res, errChallengeInvalid); err != nil {
			return err
		}
		token, err = s.insertSession(r.Context(), tx, userID)
		return err
	})
	switch {
	case errors.Is(err, errChallengeInvalid):
		fail()
		return
	case err == nil && wrong:
		writeError(w, http.StatusUnauthorized, apierr.CodeWrongTwoFactorCode, errWrongCode.Error())
		return
	case err != nil:
		giveAll(perIP)
		giveAll(account)
		serverError(w, r, err)
		return
	}
	giveAll(perIP)
	giveAll(account)

	u, err := s.userByID(r.Context(), userID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if old := s.sessionToken(r); old != "" {
		if err := s.deleteSession(r.Context(), old); err != nil {
			slog.WarnContext(r.Context(), "auth: deleting the replaced session", "err", err)
		}
	}
	s.setSessionCookie(w, token)
	s.writeMe(w, r, http.StatusOK, u)
}

func (s *Service) userByID(ctx context.Context, id int64) (User, error) {
	u := User{ID: id}
	err := s.db.QueryRowContext(ctx, `SELECT email, name FROM users WHERE id = ?`, id).Scan(&u.Email, &u.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, errNotFound
	}
	return u, err
}

// handleClearTwoFactor clears a member's two-factor login, for a member who lost both their
// authenticator and their recovery codes. The rule is the password reset link's: the admin
// must administer every workspace the member belongs to, and can't do it to their own
// account. The member's sessions end, so whoever held one of them is out too.
func (s *Service) handleClearTwoFactor(w http.ResponseWriter, r *http.Request) {
	m, _ := MembershipFrom(r.Context())
	admin, _ := UserFrom(r.Context())
	userID, ok := pathID(w, r, "user", apierr.CodeMemberNotFound, "member not found")
	if !ok {
		return
	}
	err := s.write(r.Context(), func(tx *sql.Tx) error {
		if _, err := roleIn(r.Context(), tx, m.WorkspaceID, userID); err != nil {
			return err
		}
		if userID == admin.ID {
			return errClearOwn
		}
		if all, err := administersAll(r.Context(), tx, admin.ID, userID); err != nil {
			return err
		} else if !all {
			return errClearElsewhere
		}
		if on, err := s.twoFactorOn(r.Context(), tx, userID); err != nil {
			return err
		} else if !on {
			return errTwoFactorOff
		}
		if err := dropTwoFactor(r.Context(), tx, userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(r.Context(), `DELETE FROM sessions WHERE user_id = ?`, userID)
		return err
	})
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, apierr.CodeMemberNotFound, "member not found")
		return
	case errors.Is(err, errClearOwn):
		writeError(w, http.StatusBadRequest, apierr.CodeTwoFactorClearOwn, err.Error())
		return
	case errors.Is(err, errClearElsewhere):
		writeError(w, http.StatusForbidden, apierr.CodeTwoFactorClearElsewhere, err.Error())
		return
	case errors.Is(err, errTwoFactorOff):
		writeError(w, http.StatusConflict, apierr.CodeTwoFactorNotOn, err.Error())
		return
	case err != nil:
		serverError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "a member's two-factor login cleared", "user_id", userID, "by", admin.ID, "workspace_id", m.WorkspaceID)
	w.WriteHeader(http.StatusNoContent)
}

// administersAll reports whether by is an admin of every workspace userID belongs to: the
// rule for acting on someone's sign-in (a reset link, clearing their second factor), so that
// an admin of one workspace can't take over an account that reaches another.
func administersAll(ctx context.Context, tx *sql.Tx, by, userID int64) (bool, error) {
	var elsewhere bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM memberships t
			WHERE t.user_id = ? AND NOT EXISTS (
				SELECT 1 FROM memberships a
				WHERE a.workspace_id = t.workspace_id AND a.user_id = ? AND a.role = ?))`,
		userID, by, RoleAdmin).Scan(&elsewhere)
	return !elsewhere, err
}

// markTwoFactor sets which of members have two-factor login on, for an admin to see.
func (s *Service) markTwoFactor(ctx context.Context, workspaceID int64, members []Member) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.user_id FROM user_totp t JOIN memberships m ON m.user_id = t.user_id
		WHERE m.workspace_id = ? AND t.enabled_at IS NOT NULL`, workspaceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	on := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		on[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range members {
		v := on[members[i].ID]
		members[i].TwoFactor = &v
	}
	return nil
}
