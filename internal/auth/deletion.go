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

var (
	errDeletionPending  = errors.New("this account is waiting to be deleted: an operator can cancel that")
	errDeletionNotFound = errors.New("this account isn't waiting to be deleted")
	errDeletionPurging  = errors.New("this account is being deleted now: it can't be cancelled any more")
	errReauth           = errors.New("sign in again with your provider, then try again")
)

const freshLogin = 5 * time.Minute

const defaultDeletionGrace = 14 * 24 * time.Hour

type DeletionScheduler interface {
	SchedulePurge(ctx context.Context, tx *sql.Tx, userID, workspaceID int64, at time.Time) error
	CancelPurge(ctx context.Context, tx *sql.Tx, userID int64) error
}

func (s *Service) SetDeletionScheduler(d DeletionScheduler) { s.deletions = d }

type DeletionBlocker struct {
	WorkspaceID int64  `json:"workspace_id"`
	Workspace   string `json:"workspace"`
	Operator    bool   `json:"operator"`
}

func DeletionBlockers(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, userID int64) ([]DeletionBlocker, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT w.id, w.name, w.is_setup
		FROM memberships m JOIN workspaces w ON w.id = m.workspace_id
		WHERE m.user_id = ? AND m.role = ? AND NOT EXISTS (
			SELECT 1 FROM memberships o JOIN users u ON u.id = o.user_id
			WHERE o.workspace_id = m.workspace_id AND o.role = ? AND o.user_id <> m.user_id AND u.disabled_at IS NULL)
		ORDER BY w.is_setup DESC, w.id`, userID, RoleAdmin, RoleAdmin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeletionBlocker{}
	for rows.Next() {
		var b DeletionBlocker
		if err := rows.Scan(&b.WorkspaceID, &b.Workspace, &b.Operator); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

type blockedError struct{ b DeletionBlocker }

func (e *blockedError) Error() string {
	return "a workspace needs at least one admin in use: make someone else an admin of " + e.b.Workspace + " first"
}

func writeBlocked(w http.ResponseWriter, e *blockedError) {
	code := apierr.CodeDeletionLastAdmin
	if e.b.Operator {
		code = apierr.CodeDeletionLastOperator
	}
	writeError(w, http.StatusConflict, code, e.Error(), apierr.Params{"workspace": e.b.Workspace})
}

func (s *Service) deletionRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/me/deletion", refuseTokens(s.RequireUser(http.HandlerFunc(s.handleDeletionStatus)).ServeHTTP))
	mux.Handle("POST /api/me/deletion", refuseTokens(s.RequireUser(http.HandlerFunc(s.handleDeleteOwn)).ServeHTTP))
	mux.Handle("GET /api/workspaces/{workspace}/account-deletions",
		refuseTokens(s.RequireOperator(http.HandlerFunc(s.handlePendingDeletions)).ServeHTTP))
	mux.Handle("DELETE /api/workspaces/{workspace}/account-deletions/{user}",
		refuseTokens(s.RequireOperator(http.HandlerFunc(s.handleCancelDeletion)).ServeHTTP))
}

func (s *Service) graceOf() time.Duration {
	if s.opts.AccountDeletionGrace > 0 {
		return s.opts.AccountDeletionGrace
	}
	return defaultDeletionGrace
}

type DeletionStatus struct {
	GraceDays int               `json:"grace_days"`
	Method    string            `json:"method"`
	TwoFactor bool              `json:"two_factor"`
	Fresh     bool              `json:"fresh"`
	Blockers  []DeletionBlocker `json:"blockers" apitypes:"nonnull"`
}

func (s *Service) handleDeletionStatus(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	_, hash, err := s.userByEmail(r.Context(), u.Email)
	if err != nil {
		serverError(w, r, err)
		return
	}
	out := DeletionStatus{GraceDays: int(s.graceOf() / (24 * time.Hour)), Method: "password"}
	if hash == "" {
		out.Method = "fresh_login"
		out.Fresh, err = s.freshSession(r.Context(), s.db, u.ID, s.sessionToken(r))
	} else {
		out.TwoFactor, err = s.twoFactorOn(r.Context(), s.db, u.ID)
	}
	if err == nil {
		out.Blockers, err = DeletionBlockers(r.Context(), s.db, u.ID)
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) freshSession(ctx context.Context, q querier, userID int64, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	var created string
	err := q.QueryRowContext(ctx, `SELECT created_at FROM sessions WHERE token_hash = ? AND user_id = ? AND expires_at > ?`,
		hashToken(token), userID, stamp(s.now())).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	at, err := time.Parse(timeFormat, created)
	if err != nil {
		return false, err
	}
	return !at.Before(s.now().Add(-freshLogin)), nil
}

type DeletionReply struct {
	PurgeAt string `json:"purge_at"`
}

func (s *Service) handleDeleteOwn(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var purgeAt time.Time
	ok := s.ownReauth(w, r, u, req.Password, req.Code, func(tx *sql.Tx) (err error) {
		purgeAt, err = s.requestDeletion(r.Context(), tx, u.ID)
		return err
	})
	if !ok {
		return
	}
	slog.InfoContext(r.Context(), "an account was deleted by its person; it is purged after the grace period", "user_id", u.ID)
	s.endLive(u.ID, "")
	writeJSON(w, http.StatusAccepted, DeletionReply{PurgeAt: stamp(purgeAt)})
}

func (s *Service) ownReauth(w http.ResponseWriter, r *http.Request, u User, password, code string, act func(tx *sql.Tx) error) bool {
	ctx := r.Context()
	_, hash, err := s.userByEmail(ctx, u.Email)
	if err != nil {
		serverError(w, r, err)
		return false
	}
	if hash == "" {
		err := s.write(ctx, func(tx *sql.Tx) error {
			if fresh, err := s.freshSession(ctx, tx, u.ID, s.sessionToken(r)); err != nil {
				return err
			} else if !fresh {
				return errReauth
			}
			return act(tx)
		})
		return s.deletionFailed(w, r, err)
	}
	if !s.needKey(w, r) {
		return false
	}
	verified, ok := s.checkOwnPassword(w, r, u, password)
	if !ok {
		return false
	}
	var limits []limitKey
	if on, err := s.twoFactorOn(ctx, s.db, u.ID); err != nil {
		serverError(w, r, err)
		return false
	} else if on {
		limits = append([]limitKey{{s.codePerIP, s.clientIP(r)}}, s.accountLimits(s.clientIP(r), u.ID)...)
		if ok, wait := takeAll(limits); !ok {
			tooManyCodes(w, wait)
			return false
		}
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		if err := s.passwordUnchanged(ctx, tx, u.ID, verified); err != nil {
			return err
		}
		if on, err := s.twoFactorOn(ctx, tx, u.ID); err != nil {
			return err
		} else if on {
			if err := s.useCode(ctx, tx, u.ID, code, false); err != nil {
				return err
			}
		}
		return act(tx)
	})
	if err != nil && !errors.Is(err, errWrongCode) {
		giveAll(limits)
	}
	if err == nil {
		giveAll(limits)
	}
	return s.deletionFailed(w, r, err)
}

func (s *Service) deletionFailed(w http.ResponseWriter, r *http.Request, err error) bool {
	var blocked *blockedError
	switch {
	case err == nil:
		return true
	case errors.As(err, &blocked):
		writeBlocked(w, blocked)
	case errors.Is(err, errReauth):
		writeError(w, http.StatusForbidden, apierr.CodeDeletionReauth, err.Error())
	case errors.Is(err, errDeletionPending):
		writeError(w, http.StatusConflict, apierr.CodeDeletionPending, err.Error())
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, apierr.CodeMemberNotFound, "member not found")
	case errors.Is(err, errAccountDisabled):
		accountDisabled(w)
	case errors.Is(err, errWrongCode), errors.Is(err, errPasswordChanged), errors.Is(err, errTwoFactorOff):
		failTwoFactor(w, r, err)
	default:
		serverError(w, r, err)
	}
	return false
}

func (s *Service) requestDeletion(ctx context.Context, tx *sql.Tx, userID int64) (time.Time, error) {
	var disabled bool
	var pending bool
	err := tx.QueryRowContext(ctx, `
		SELECT u.disabled_at IS NOT NULL, EXISTS (SELECT 1 FROM account_deletions d WHERE d.user_id = u.id)
		FROM users u WHERE u.id = ?`, userID).Scan(&disabled, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, errNotFound
	}
	if err != nil {
		return time.Time{}, err
	}
	if pending {
		return time.Time{}, errDeletionPending
	}
	blockers, err := DeletionBlockers(ctx, tx, userID)
	if err != nil {
		return time.Time{}, err
	}
	if len(blockers) > 0 {
		return time.Time{}, &blockedError{b: blockers[0]}
	}
	now := s.now()
	purgeAt := now.Add(s.graceOf())
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO account_deletions (user_id, requested_at, purge_at, was_disabled)
		VALUES (?, ?, ?, ?)`, userID, stamp(now), stamp(purgeAt), disabled); err != nil {
		return time.Time{}, err
	}
	if !disabled {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET disabled_at = ?, disabled_by = NULL WHERE id = ?`,
			stamp(now), userID); err != nil {
			return time.Time{}, err
		}
	}
	ended, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return time.Time{}, err
	}
	sessions, err := ended.RowsAffected()
	if err != nil {
		return time.Time{}, err
	}
	if err := s.endSignIns(ctx, tx, userID); err != nil {
		return time.Time{}, err
	}
	workspaces, err := workspaceIDs(ctx, tx, userID)
	if err != nil {
		return time.Time{}, err
	}
	if s.deletions != nil {
		if err := s.deletions.SchedulePurge(ctx, tx, userID, workspaces.home, purgeAt); err != nil {
			return time.Time{}, err
		}
	}
	for _, ws := range workspaces.all {
		if err := audit.Record(ctx, tx, audit.Event{Workspace: ws, Actor: userID, Action: audit.AccountDeletionRequested,
			TargetType: audit.TargetUser, Target: userID,
			Details: audit.Details{"purge_days": int64(s.graceOf() / (24 * time.Hour)), "logins_ended": sessions}}); err != nil {
			return time.Time{}, err
		}
	}
	return purgeAt, nil
}

type userWorkspaces struct {
	all  []int64
	home int64
}

func workspaceIDs(ctx context.Context, tx *sql.Tx, userID int64) (userWorkspaces, error) {
	var out userWorkspaces
	rows, err := tx.QueryContext(ctx, `SELECT m.workspace_id FROM memberships m JOIN workspaces w ON w.id = m.workspace_id
		WHERE m.user_id = ? ORDER BY w.is_setup DESC, w.id`, userID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return out, err
		}
		out.all = append(out.all, id)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	_ = rows.Close()
	if len(out.all) > 0 {
		out.home = out.all[0]
		return out, nil
	}
	out.all = []int64{0}
	err = tx.QueryRowContext(ctx, `SELECT id FROM workspaces ORDER BY is_setup DESC, id LIMIT 1`).Scan(&out.home)
	return out, err
}

type PendingDeletion struct {
	User        User   `json:"user"`
	RequestedAt string `json:"requested_at"`
	PurgeAt     string `json:"purge_at"`
	Purging     bool   `json:"purging"`
}

func (s *Service) handlePendingDeletions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT u.id, u.email, u.name, d.requested_at, d.purge_at, d.purge_started_at IS NOT NULL
		FROM account_deletions d JOIN users u ON u.id = d.user_id
		ORDER BY d.purge_at, u.id`)
	if err != nil {
		serverError(w, r, err)
		return
	}
	defer rows.Close()
	out := []PendingDeletion{}
	for rows.Next() {
		var p PendingDeletion
		if err := rows.Scan(&p.User.ID, &p.User.Email, &p.User.Name, &p.RequestedAt, &p.PurgeAt, &p.Purging); err != nil {
			serverError(w, r, err)
			return
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]PendingDeletion{"deletions": out})
}

func (s *Service) handleCancelDeletion(w http.ResponseWriter, r *http.Request) {
	operator, _ := UserFrom(r.Context())
	userID, ok := pathID(w, r, "user", apierr.CodeDeletionNotPending, "this account isn't waiting to be deleted")
	if !ok {
		return
	}
	if !readEmptyBody(w, r) {
		return
	}
	err := s.write(r.Context(), func(tx *sql.Tx) error {
		if off, err := s.isDisabled(r.Context(), tx, operator.ID); err != nil {
			return err
		} else if off {
			return errAccountDisabled
		}
		if ok, err := isOperator(r.Context(), tx, operator.ID); err != nil {
			return err
		} else if !ok {
			return errNotOperator
		}
		return s.cancelDeletion(r.Context(), tx, userID, operator.ID)
	})
	switch {
	case errors.Is(err, errDeletionNotFound):
		writeError(w, http.StatusNotFound, apierr.CodeDeletionNotPending, err.Error())
		return
	case errors.Is(err, errDeletionPurging):
		writeError(w, http.StatusConflict, apierr.CodeDeletionPurging, err.Error())
		return
	case errors.Is(err, errAccountDisabled):
		accountDisabled(w)
		return
	case errors.Is(err, errNotOperator):
		writeError(w, http.StatusForbidden, apierr.CodeOperatorOnly, "only the server's operator can do this")
		return
	case err != nil:
		serverError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "an account's deletion was cancelled", "user_id", userID, "by", operator.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) cancelDeletion(ctx context.Context, tx *sql.Tx, userID, by int64) error {
	var wasDisabled bool
	err := tx.QueryRowContext(ctx, `DELETE FROM account_deletions WHERE user_id = ? AND purge_started_at IS NULL RETURNING was_disabled`,
		userID).Scan(&wasDisabled)
	if errors.Is(err, sql.ErrNoRows) {
		if pending, perr := isDeleting(ctx, tx, userID); perr != nil {
			return perr
		} else if pending {
			return errDeletionPurging
		}
		return errDeletionNotFound
	}
	if err != nil {
		return err
	}
	if !wasDisabled {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET disabled_at = NULL, disabled_by = NULL WHERE id = ?`, userID); err != nil {
			return err
		}
	}
	if s.deletions != nil {
		if err := s.deletions.CancelPurge(ctx, tx, userID); err != nil {
			return err
		}
	}
	workspaces, err := workspaceIDs(ctx, tx, userID)
	if err != nil {
		return err
	}
	for _, ws := range workspaces.all {
		if err := audit.Record(ctx, tx, audit.Event{Workspace: ws, Actor: by, Action: audit.AccountDeletionCancelled,
			TargetType: audit.TargetUser, Target: userID}); err != nil {
			return err
		}
	}
	return nil
}

func isDeleting(ctx context.Context, q querier, userID int64) (bool, error) {
	var pending bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM account_deletions WHERE user_id = ?)`, userID).Scan(&pending)
	return pending, err
}
