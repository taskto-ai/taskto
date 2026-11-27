package auth

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/ratelimit"
)

var (
	errWrongPassword   = errors.New("the current password is wrong")
	errPasswordChanged = errors.New("the password changed meanwhile: try again")
	errResetInvalid    = errors.New("the reset link is unknown, used or expired: ask an admin for a new one")
	errResetOwn        = errors.New("change your own password in Settings, with your current one")
	errResetElsewhere  = errors.New("this member also belongs to a workspace you aren't an admin of: only someone who administers all of their workspaces can reset their password")
)

func (s *Service) passwordRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/me/password", refuseTokens(s.RequireUser(http.HandlerFunc(s.handleChangePassword)).ServeHTTP))
	mux.Handle("POST /api/workspaces/{workspace}/members/{user}/password-reset",
		refuseTokens(s.RequireAdmin(http.HandlerFunc(s.handleCreateReset)).ServeHTTP))
	mux.Handle("POST /api/password-reset", refuseTokens(s.handleUseReset))
	mux.Handle("POST /api/password-reset/request", refuseTokens(s.handleRequestReset))
}

func (s *Service) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	var req struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Current == "" {
		writeError(w, http.StatusBadRequest, apierr.CodeCurrentPasswordRequired, "the current password is required")
		return
	}
	if e := checkPassword(req.New); e != nil {
		apierr.WriteError(w, e)
		return
	}
	_, hash, err := s.userByEmail(r.Context(), u.Email)
	if err != nil {
		serverError(w, r, err)
		return
	}
	ip := s.clientIP(r)
	limits := []struct {
		l   *ratelimit.Limiter
		key string
	}{
		{s.perIP, ip},
		{s.perAccountIP, u.Email + "|" + ip},
		{s.perAccount, u.Email},
	}
	for i, lim := range limits {
		if ok, wait := lim.l.Take(lim.key); !ok {
			for _, taken := range limits[:i] {
				taken.l.Give(taken.key)
			}
			limited(w, wait, apierr.CodeTooManyPasswords, "too many wrong passwords: try again later")
			return
		}
	}
	var match bool
	if hash != "" {
		match, err = s.hasher.verify(r.Context(), req.Current, hash)
	}
	if match || err != nil {
		for _, lim := range limits {
			lim.l.Give(lim.key)
		}
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !match {
		audit.Log(r.Context(), s.db, audit.Event{Actor: u.ID, Action: audit.PasswordChangeFailed,
			TargetType: audit.TargetUser, Target: u.ID})
		writeError(w, http.StatusForbidden, apierr.CodeWrongPassword, errWrongPassword.Error())
		return
	}
	newHash, err := s.hasher.hash(r.Context(), req.New)
	if err != nil {
		serverError(w, r, err)
		return
	}
	token, err := s.changePassword(r.Context(), u.ID, hash, newHash)
	if errors.Is(err, errPasswordChanged) {
		writeError(w, http.StatusConflict, apierr.CodePasswordChanged, err.Error())
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "password changed; the user's other sessions are signed out", "user_id", u.ID)
	s.setSessionCookie(w, token)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleCreateReset(w http.ResponseWriter, r *http.Request) {
	if s.opts.DisablePasswordLogin {
		passwordLoginDisabled(w)
		return
	}
	m, _ := MembershipFrom(r.Context())
	admin, _ := UserFrom(r.Context())
	userID, ok := pathID(w, r, "user", apierr.CodeMemberNotFound, "member not found")
	if !ok {
		return
	}
	if !readEmptyBody(w, r) {
		return
	}
	token, expires, err := s.createReset(r.Context(), m.WorkspaceID, admin.ID, userID)
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, apierr.CodeMemberNotFound, "member not found")
		return
	case errors.Is(err, errResetOwn):
		writeError(w, http.StatusBadRequest, apierr.CodeResetOwn, err.Error())
		return
	case errors.Is(err, errResetElsewhere):
		writeError(w, http.StatusForbidden, apierr.CodeResetElsewhere, err.Error())
		return
	case errors.Is(err, errAccountDisabled):
		accountDisabled(w)
		return
	case err != nil:
		serverError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "password reset link made", "user_id", userID, "by", admin.ID, "workspace_id", m.WorkspaceID)
	writeJSON(w, http.StatusCreated, map[string]string{"token": token, "expires_at": expires})
}

func (s *Service) handleUseReset(w http.ResponseWriter, r *http.Request) {
	if s.opts.DisablePasswordLogin {
		passwordLoginDisabled(w)
		return
	}
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Token == "" || len(req.Token) > 128 {
		writeError(w, http.StatusNotFound, apierr.CodeResetInvalid, errResetInvalid.Error())
		return
	}
	if e := checkPassword(req.Password); e != nil {
		apierr.WriteError(w, e)
		return
	}
	if _, err := s.resetUser(r.Context(), s.db, req.Token); err != nil {
		s.failReset(w, r, err)
		return
	}
	hash, err := s.hasher.hash(r.Context(), req.Password)
	if err != nil {
		serverError(w, r, err)
		return
	}
	u, out, err := s.useReset(r.Context(), req.Token, hash)
	if err != nil {
		s.failReset(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "password set with a reset link; the user's sessions are signed out", "user_id", u.ID)
	if out.challenge != "" {
		writeChallenge(w, out)
		return
	}
	if old := s.sessionToken(r); old != "" {
		if err := s.deleteSession(r.Context(), old); err != nil {
			slog.WarnContext(r.Context(), "auth: deleting the replaced session", "err", err)
		}
	}
	s.setSessionCookie(w, out.session)
	s.writeMe(w, r, http.StatusOK, u)
}

func (s *Service) failReset(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errResetInvalid) {
		writeError(w, http.StatusNotFound, apierr.CodeResetInvalid, err.Error())
		return
	}
	if errors.Is(err, errAccountDisabled) {
		accountDisabled(w)
		return
	}
	serverError(w, r, err)
}

func readEmptyBody(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength == 0 {
		return true
	}
	var req struct{}
	return readJSON(w, r, &req)
}

func limited(w http.ResponseWriter, wait time.Duration, code apierr.Code, msg string) {
	setRetryAfter(w, wait)
	writeError(w, http.StatusTooManyRequests, code, msg)
}

func (s *Service) changePassword(ctx context.Context, userID int64, current, newHash string) (string, error) {
	var token string
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ? AND password_hash = ?`,
			newHash, userID, current)
		if err != nil {
			return err
		}
		if err := mustAffect(res, errPasswordChanged); err != nil {
			return err
		}
		if token, err = s.signOutEverywhere(ctx, tx, userID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Actor: userID, Action: audit.PasswordChanged,
			TargetType: audit.TargetUser, Target: userID})
	})
	if err == nil {
		s.endLive(userID, "")
	}
	return token, err
}

func (s *Service) signOutEverywhere(ctx context.Context, tx *sql.Tx, userID int64) (string, error) {
	if err := s.endSignIns(ctx, tx, userID); err != nil {
		return "", err
	}
	return s.insertSession(ctx, tx, userID)
}

func (s *Service) endSignIns(ctx context.Context, tx *sql.Tx, userID int64) error {
	for _, q := range []string{
		`DELETE FROM sessions WHERE user_id = ?`,
		`DELETE FROM password_resets WHERE user_id = ?`,
		`DELETE FROM login_challenges WHERE user_id = ?`,
		`DELETE FROM user_totp WHERE user_id = ? AND enabled_at IS NULL`,
	} {
		if _, err := tx.ExecContext(ctx, q, userID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) createReset(ctx context.Context, workspaceID, by, userID int64) (token, expires string, err error) {
	now := s.now()
	expires = stamp(now.Add(s.opts.ResetTTL))
	token, hash := newToken()
	err = s.write(ctx, func(tx *sql.Tx) error {
		if _, err := roleIn(ctx, tx, workspaceID, userID); err != nil {
			return err
		}
		if userID == by {
			return errResetOwn
		}
		if all, err := administersAll(ctx, tx, by, userID); err != nil {
			return err
		} else if !all {
			return errResetElsewhere
		}
		if off, err := s.isDisabled(ctx, tx, by); err != nil {
			return err
		} else if off {
			return errAccountDisabled
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM password_resets WHERE user_id = ?`, userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO password_resets (user_id, token_hash, created_by, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?)`, userID, hash, by, stamp(now), expires)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Actor: by, Action: audit.PasswordResetCreated,
			TargetType: audit.TargetUser, Target: userID})
	})
	if err != nil {
		return "", "", err
	}
	return token, expires, nil
}

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

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Service) resetUser(ctx context.Context, q querier, token string) (User, error) {
	var u User
	var disabled bool
	err := q.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.name, u.disabled_at IS NOT NULL
		FROM password_resets p JOIN users u ON u.id = p.user_id
		WHERE p.token_hash = ? AND p.expires_at > ?`, hashToken(token), stamp(s.now())).
		Scan(&u.ID, &u.Email, &u.Name, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, errResetInvalid
	}
	if err == nil && disabled {
		return User{}, errAccountDisabled
	}
	return u, err
}

func (s *Service) useReset(ctx context.Context, token, passwordHash string) (User, signIn, error) {
	var u User
	var out signIn
	err := s.write(ctx, func(tx *sql.Tx) error {
		var err error
		if u, err = s.resetUser(ctx, tx, token); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, u.ID); err != nil {
			return err
		}
		if err := s.endSignIns(ctx, tx, u.ID); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{Actor: u.ID, Action: audit.PasswordResetUsed,
			TargetType: audit.TargetUser, Target: u.ID}); err != nil {
			return err
		}
		out, err = s.issueSignIn(ctx, tx, u.ID)
		return err
	})
	if err == nil {
		s.endLive(u.ID, "")
	}
	return u, out, err
}
