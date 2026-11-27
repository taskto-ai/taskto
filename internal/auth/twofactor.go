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
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/ratelimit"
)

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
	challengeTTL      = 5 * time.Minute
	challengeAttempts = 5
	enrolmentTTL      = 15 * time.Minute
)

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

type limitKey struct {
	l   *ratelimit.Limiter
	key string
}

func takeAll(ls []limitKey) (bool, time.Duration) {
	for i, lim := range ls {
		if ok, wait := lim.l.Take(lim.key); !ok {
			giveAll(ls[:i])
			return false, wait
		}
	}
	return true, 0
}

func giveAll(ls []limitKey) {
	for _, lim := range ls {
		lim.l.Give(lim.key)
	}
}

func tooManyCodes(w http.ResponseWriter, wait time.Duration) {
	limited(w, wait, apierr.CodeTooManyTwoFactorCodes, "too many wrong codes: try again later")
}

func (s *Service) accountLimits(ip string, userID int64) []limitKey {
	id := strconv.FormatInt(userID, 10)
	return []limitKey{{s.codePerAccountIP, id + "|" + ip}, {s.codePerAccount, id}}
}

func (s *Service) sealAD(userID int64) []byte {
	return []byte("taskto auth totp v1 user " + strconv.FormatInt(userID, 10))
}

func (s *Service) recoveryHash(userID int64, code string) string {
	return hex.EncodeToString(s.key.MAC("auth recovery code v1", []byte(strconv.FormatInt(userID, 10)+":"+code)))
}

func (s *Service) twoFactorOn(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, userID int64) (bool, error) {
	var on bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM user_totp WHERE user_id = ? AND enabled_at IS NOT NULL)`, userID).Scan(&on)
	return on, err
}

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

func (s *Service) checkOwnPassword(w http.ResponseWriter, r *http.Request, u User, password string) (string, bool) {
	if password == "" {
		writeError(w, http.StatusBadRequest, apierr.CodeCurrentPasswordRequired, "the current password is required")
		return "", false
	}
	_, hash, err := s.userByEmail(r.Context(), u.Email)
	if err != nil {
		serverError(w, r, err)
		return "", false
	}
	ip := s.clientIP(r)
	limits := []limitKey{{s.perIP, ip}, {s.perAccountIP, u.Email + "|" + ip}, {s.perAccount, u.Email}}
	if ok, wait := takeAll(limits); !ok {
		limited(w, wait, apierr.CodeTooManyPasswords, "too many wrong passwords: try again later")
		return "", false
	}
	if hash == "" {
		giveAll(limits)
		writeError(w, http.StatusForbidden, apierr.CodeTwoFactorNoPassword, "this account has no password to protect with a second factor")
		return "", false
	}
	match, err := s.hasher.verify(r.Context(), password, hash)
	if match || err != nil {
		giveAll(limits)
	}
	if err != nil {
		serverError(w, r, err)
		return "", false
	}
	if !match {
		writeError(w, http.StatusForbidden, apierr.CodeWrongPassword, errWrongPassword.Error())
		return "", false
	}
	s.at("own-password-verified")
	return hash, true
}

func failTwoFactor(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errWrongCode):
		writeError(w, http.StatusForbidden, apierr.CodeWrongTwoFactorCode, err.Error())
	case errors.Is(err, errTwoFactorOn):
		writeError(w, http.StatusConflict, apierr.CodeTwoFactorAlreadyOn, err.Error())
	case errors.Is(err, errTwoFactorOff):
		writeError(w, http.StatusConflict, apierr.CodeTwoFactorNotOn, err.Error())
	case errors.Is(err, errNotStarted):
		writeError(w, http.StatusConflict, apierr.CodeTwoFactorNotStarted, err.Error())
	case errors.Is(err, errPasswordChanged):
		writeError(w, http.StatusConflict, apierr.CodePasswordChanged, err.Error())
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
	var on bool
	var left int
	err := s.db.QueryRowContext(r.Context(), `
		SELECT EXISTS (SELECT 1 FROM user_totp WHERE user_id = ?1 AND enabled_at IS NOT NULL),
		       CASE WHEN EXISTS (SELECT 1 FROM user_totp WHERE user_id = ?1 AND enabled_at IS NOT NULL)
		            THEN (SELECT count(*) FROM user_recovery_codes WHERE user_id = ?1 AND used_at IS NULL) ELSE 0 END`,
		u.ID).Scan(&on, &left)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": on, "recovery_codes_left": left})
}

func (s *Service) handleTwoFactorEnrol(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) || !s.needKey(w, r) {
		return
	}
	if s.opts.DisablePasswordLogin {
		passwordLoginDisabled(w)
		return
	}
	verified, ok := s.checkOwnPassword(w, r, u, req.Password)
	if !ok {
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
		if err := s.passwordUnchanged(r.Context(), tx, u.ID, verified); err != nil {
			return err
		}
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
		if codes, err = s.replaceRecoveryCodes(r.Context(), tx, u.ID); err != nil {
			return err
		}
		return audit.Record(r.Context(), tx, audit.Event{Actor: u.ID, Action: audit.TwoFactorEnabled,
			TargetType: audit.TargetUser, Target: u.ID})
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

func (s *Service) ownFactorAction(w http.ResponseWriter, r *http.Request, password, code string, act func(tx *sql.Tx, userID int64) error) bool {
	u, _ := UserFrom(r.Context())
	if !s.needKey(w, r) {
		return false
	}
	verified, ok := s.checkOwnPassword(w, r, u, password)
	if !ok {
		return false
	}
	limits := append([]limitKey{{s.codePerIP, s.clientIP(r)}}, s.accountLimits(s.clientIP(r), u.ID)...)
	if ok, wait := takeAll(limits); !ok {
		tooManyCodes(w, wait)
		return false
	}
	err := s.write(r.Context(), func(tx *sql.Tx) error {
		if err := s.passwordUnchanged(r.Context(), tx, u.ID, verified); err != nil {
			return err
		}
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
		if err := dropTwoFactor(r.Context(), tx, userID); err != nil {
			return err
		}
		return audit.Record(r.Context(), tx, audit.Event{Actor: userID, Action: audit.TwoFactorDisabled,
			TargetType: audit.TargetUser, Target: userID})
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
		if codes, err = s.replaceRecoveryCodes(r.Context(), tx, userID); err != nil {
			return err
		}
		return audit.Record(r.Context(), tx, audit.Event{Actor: userID, Action: audit.TwoFactorRecoveryCodesReplaced,
			TargetType: audit.TargetUser, Target: userID})
	})
	if !ok {
		return
	}
	slog.InfoContext(r.Context(), "recovery codes made again", "user_id", u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

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

type signIn struct {
	session   string
	challenge string
	expires   string
}

func (s *Service) issueSignIn(ctx context.Context, tx *sql.Tx, userID int64) (signIn, error) {
	if off, err := s.isDisabled(ctx, tx, userID); err != nil {
		return signIn{}, err
	} else if off {
		return signIn{}, errAccountDisabled
	}
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

func (s *Service) passwordSignIn(ctx context.Context, userID int64, verifiedHash string) (signIn, error) {
	var out signIn
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := s.passwordUnchanged(ctx, tx, userID, verifiedHash); err != nil {
			return err
		}
		var err error
		if out, err = s.issueSignIn(ctx, tx, userID); err != nil || out.session == "" {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Actor: userID, Action: audit.LoginSucceeded,
			TargetType: audit.TargetUser, Target: userID, Details: audit.Details{"method": "password"}})
	})
	return out, err
}

func (s *Service) passwordUnchanged(ctx context.Context, tx *sql.Tx, userID int64, verified string) error {
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&current); err != nil {
		return err
	}
	if current != verified {
		return errPasswordChanged
	}
	return nil
}

func writeChallenge(w http.ResponseWriter, out signIn) {
	writeJSON(w, http.StatusOK, map[string]any{"two_factor_required": true, "challenge": out.challenge, "expires_at": out.expires})
}

func (s *Service) handleLoginTwoFactor(w http.ResponseWriter, r *http.Request) {
	if s.opts.DisablePasswordLogin {
		passwordLoginDisabled(w)
		return
	}
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
		giveAll(perIP)
		tooManyCodes(w, wait)
		return
	}
	s.at("challenge-found")
	var token string
	var u User
	var wrong bool
	err = s.write(r.Context(), func(tx *sql.Tx) error {
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
		if token, err = s.insertSession(r.Context(), tx, userID); err != nil {
			return err
		}
		if err := audit.Record(r.Context(), tx, audit.Event{Actor: userID, Action: audit.LoginSucceeded,
			TargetType: audit.TargetUser, Target: userID, Details: audit.Details{"method": "password"}}); err != nil {
			return err
		}
		u, err = s.userByID(r.Context(), tx, userID)
		return err
	})
	switch {
	case errors.Is(err, errChallengeInvalid):
		fail()
		return
	case errors.Is(err, errAccountDisabled):
		s.loginFailed(r, userID, "account_disabled")
		accountDisabled(w)
		return
	case err == nil && wrong:
		s.loginFailed(r, userID, "wrong_code")
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

	if old := s.sessionToken(r); old != "" {
		if err := s.deleteSession(r.Context(), old); err != nil {
			slog.WarnContext(r.Context(), "auth: deleting the replaced session", "err", err)
		}
	}
	s.setSessionCookie(w, token)
	s.writeMe(w, r, http.StatusOK, u)
}

func (s *Service) userByID(ctx context.Context, q querier, id int64) (User, error) {
	u := User{ID: id}
	err := q.QueryRowContext(ctx, `SELECT email, name FROM users WHERE id = ?`, id).Scan(&u.Email, &u.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, errNotFound
	}
	return u, err
}

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
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
			return err
		}
		return audit.Record(r.Context(), tx, audit.Event{Workspace: m.WorkspaceID, Actor: admin.ID, Action: audit.TwoFactorCleared,
			TargetType: audit.TargetUser, Target: userID})
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
	s.endLive(userID, "")
	slog.InfoContext(r.Context(), "a member's two-factor login cleared", "user_id", userID, "by", admin.ID, "workspace_id", m.WorkspaceID)
	w.WriteHeader(http.StatusNoContent)
}

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
