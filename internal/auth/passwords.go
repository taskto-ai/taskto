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
)

// Changing a password, and reset links (docs/api.md#passwords).
//
//   - A user changes their own password with the current one. The routes act on the caller
//     only: there is no user in the path or the body.
//   - An admin makes a reset link for a member, shown once, to send them. There is no email
//     sending: the admin passes the link on, as with invites. Whoever has the link can set
//     the password, so an admin may make one only for a member of nothing but workspaces they
//     administer; an admin of one workspace can't take over an account that reaches another.
//   - Either way the user's sessions all end, and the browser that set the new password gets
//     a new one. Access tokens stay: they are listed in Settings, and revoked there.

var (
	errWrongPassword   = errors.New("the current password is wrong")
	errPasswordChanged = errors.New("the password changed meanwhile: try again")
	errResetInvalid    = errors.New("the reset link is unknown, used or expired: ask an admin for a new one")
	errResetOwn        = errors.New("change your own password in Settings, with your current one")
	errResetElsewhere  = errors.New("this member also belongs to a workspace you aren't an admin of: only someone who administers all of their workspaces can reset their password")
)

func (s *Service) passwordRoutes(mux *http.ServeMux) {
	// RequireUser and RequireAdmin take no access tokens; refuseTokens says so up front.
	mux.Handle("POST /api/me/password", refuseTokens(s.RequireUser(http.HandlerFunc(s.handleChangePassword)).ServeHTTP))
	mux.Handle("POST /api/workspaces/{workspace}/members/{user}/password-reset",
		refuseTokens(s.RequireAdmin(http.HandlerFunc(s.handleCreateReset)).ServeHTTP))
	mux.Handle("POST /api/password-reset", refuseTokens(s.handleUseReset))
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
	// A wrong current password counts as a failed login: a session someone else got hold of
	// is no faster a way to guess the password than the login form.
	ip := s.clientIP(r)
	limits := []struct {
		l   *limiter
		key string
	}{
		{s.perIP, ip},
		{s.perAccountIP, u.Email + "|" + ip},
		{s.perAccount, u.Email},
	}
	for i, lim := range limits {
		if ok, wait := lim.l.take(lim.key); !ok {
			for _, taken := range limits[:i] {
				taken.l.give(taken.key)
			}
			limited(w, wait, apierr.CodeTooManyPasswords, "too many wrong passwords: try again later")
			return
		}
	}
	// An account made by signing in with a provider has no password to match.
	var match bool
	if hash != "" {
		match, err = s.hasher.verify(r.Context(), req.Current, hash)
	}
	if match || err != nil {
		for _, lim := range limits {
			lim.l.give(lim.key)
		}
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	if !match {
		audit.Log(r.Context(), s.db, audit.Event{Actor: u.ID, Action: audit.PasswordChangeFailed,
			TargetType: audit.TargetUser, Target: u.ID})
		// 403, not 401: the session is fine, and 401 would send the web app to the login.
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
	// A link nobody could use: with passwords off, using one is refused too.
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
	case err != nil:
		serverError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "password reset link made", "user_id", userID, "by", admin.ID, "workspace_id", m.WorkspaceID)
	// The only time the token is shown: the database keeps its hash.
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
	// Checked before hashing, which is slow; useReset checks again in its transaction.
	if _, err := s.resetUser(r.Context(), s.db, req.Token); err != nil {
		s.failReset(w, r, err)
		return
	}
	hash, err := s.hasher.hash(r.Context(), req.Password)
	if err != nil {
		serverError(w, r, err)
		return
	}
	u, token, err := s.useReset(r.Context(), req.Token, hash)
	if err != nil {
		s.failReset(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "password set with a reset link; the user's sessions are signed out", "user_id", u.ID)
	// As at login: the browser's earlier session, whoever's it was, ends.
	if old := s.sessionToken(r); old != "" {
		if err := s.deleteSession(r.Context(), old); err != nil {
			slog.WarnContext(r.Context(), "auth: deleting the replaced session", "err", err)
		}
	}
	s.setSessionCookie(w, token)
	s.writeMe(w, r, http.StatusOK, u)
}

func (s *Service) failReset(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errResetInvalid) {
		writeError(w, http.StatusNotFound, apierr.CodeResetInvalid, err.Error())
		return
	}
	serverError(w, r, err)
}

// readEmptyBody accepts no body, or an empty JSON object, and answers 400 to anything else:
// a route that acts on its path takes nothing from the body.
func readEmptyBody(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength == 0 {
		return true
	}
	var req struct{}
	return readJSON(w, r, &req)
}

// limited answers 429 with when to try again.
func limited(w http.ResponseWriter, wait time.Duration, code apierr.Code, msg string) {
	setRetryAfter(w, wait)
	writeError(w, http.StatusTooManyRequests, code, msg)
}

// changePassword replaces userID's password hash, if it is still current, ends all their
// sessions and reset links, and returns a new session for the browser that changed it.
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
	return token, err
}

// signOutEverywhere ends userID's sessions and reset links within tx, and returns a new
// session.
func (s *Service) signOutEverywhere(ctx context.Context, tx *sql.Tx, userID int64) (string, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM password_resets WHERE user_id = ?`, userID); err != nil {
		return "", err
	}
	return s.insertSession(ctx, tx, userID)
}

// createReset makes a reset link for userID, a member of workspaceID, by the admin by, in
// place of any earlier one. It fails with errNotFound if userID isn't a member,
// errResetOwn for the admin's own account, and errResetElsewhere if userID belongs to a
// workspace by doesn't administer.
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
		var elsewhere bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM memberships t
				WHERE t.user_id = ? AND NOT EXISTS (
					SELECT 1 FROM memberships a
					WHERE a.workspace_id = t.workspace_id AND a.user_id = ? AND a.role = ?))`,
			userID, by, RoleAdmin).Scan(&elsewhere); err != nil {
			return err
		}
		if elsewhere {
			return errResetElsewhere
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
		// Never the link or its token: only that an admin made one, and for whom.
		return audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Actor: by, Action: audit.PasswordResetCreated,
			TargetType: audit.TargetUser, Target: userID})
	})
	if err != nil {
		return "", "", err
	}
	return token, expires, nil
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// resetUser returns the user a reset token is for, or errResetInvalid if it is unknown or
// expired.
func (s *Service) resetUser(ctx context.Context, q querier, token string) (User, error) {
	var u User
	err := q.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.name
		FROM password_resets p JOIN users u ON u.id = p.user_id
		WHERE p.token_hash = ? AND p.expires_at > ?`, hashToken(token), stamp(s.now())).
		Scan(&u.ID, &u.Email, &u.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, errResetInvalid
	}
	return u, err
}

// useReset sets the password of the user a reset token is for, once: it ends their sessions
// and reset links, this one included, and returns them with a new session.
func (s *Service) useReset(ctx context.Context, token, passwordHash string) (User, string, error) {
	var u User
	var session string
	err := s.write(ctx, func(tx *sql.Tx) error {
		var err error
		if u, err = s.resetUser(ctx, tx, token); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, u.ID); err != nil {
			return err
		}
		if session, err = s.signOutEverywhere(ctx, tx, u.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Actor: u.ID, Action: audit.PasswordResetUsed,
			TargetType: audit.TargetUser, Target: u.ID})
	})
	return u, session, err
}
