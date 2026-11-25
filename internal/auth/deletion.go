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

// Deleting an account (docs/api.md#deleting-an-account). A person deletes their own account, with
// their password (and a code, if they have two-factor login on) or, with no password, a login made
// minutes ago; or an admin deletes a member's, by the rule for reset links (administersAll). Either
// way the account is disabled at once, with the same switch as disabling a member, and kept for a
// grace period, after which the purge (internal/accountdeletion, run by the scheduler) deletes
// what the person owned alone and then the account. An operator can cancel it until then.
//
// This file is the account's side of it: asking, cancelling, who may, and what a deletion can't
// leave behind (a workspace with no admin in use). The purge deletes files and talks to the agent
// runtime, which this package knows nothing about, so it is the other package's, and reaches back
// here for what must hold at the time (DeletionBlockers).

var (
	errDeletionOwn       = errors.New("delete your own account from your own settings")
	errDeletionElsewhere = errors.New("this member also belongs to a workspace you aren't an admin of: only someone who administers all of their workspaces can delete their account")
	errDeletionPending   = errors.New("this account is waiting to be deleted: an operator can cancel that")
	errDeletionNotFound  = errors.New("this account isn't waiting to be deleted")
	errDeletionPurging   = errors.New("this account is being deleted now: it can't be cancelled any more")
	errReauth            = errors.New("sign in again with your provider, then try again")
)

// freshLogin is how recent a login must be to stand for a password: an account that signs in with a
// provider and has no password proves it is the person by having just signed in.
const freshLogin = 5 * time.Minute

// defaultDeletionGrace is how long a deleted account is kept, unless the operator says otherwise.
const defaultDeletionGrace = 14 * 24 * time.Hour

// DeletionScheduler is the timed work of a deletion, made in the transaction that asks for or
// cancels it (the scheduler's, package schedule, wrapped by internal/accountdeletion): this package
// can't import it, since the scheduler's access checks import this one.
type DeletionScheduler interface {
	// SchedulePurge makes the purge of userID due at at. workspaceID is a workspace the job is filed
	// under; the purge acts for no one's access, only for the server's.
	SchedulePurge(ctx context.Context, tx *sql.Tx, userID, workspaceID int64, at time.Time) error
	// CancelPurge removes it.
	CancelPurge(ctx context.Context, tx *sql.Tx, userID int64) error
}

// SetDeletionScheduler sets the scheduler of purges. It is for startup, before the service
// serves requests; without one a deletion is recorded and its purge is made by Reconcile when the
// server next starts with one.
func (s *Service) SetDeletionScheduler(d DeletionScheduler) { s.deletions = d }

// DeletionBlocker is a workspace that would be left with no admin in use by a deletion.
type DeletionBlocker struct {
	WorkspaceID int64  `json:"workspace_id"`
	Workspace   string `json:"workspace"`
	// Operator is set for the workspace the server's setup made, whose admins run the server.
	Operator bool `json:"operator"`
}

// DeletionBlockers lists the workspaces userID is an admin of where no other admin is in use: an
// account in one of them can't be deleted until someone else is made an admin there. Admins whose
// accounts are disabled (or waiting to be deleted) don't count, as everywhere a workspace's admins
// are counted (checkNotLastAdmin).
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

// blockedError is a deletion refused because it would leave a workspace without an admin in use.
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
	// These take no access tokens: they act on the whole account.
	mux.Handle("GET /api/me/deletion", refuseTokens(s.RequireUser(http.HandlerFunc(s.handleDeletionStatus)).ServeHTTP))
	mux.Handle("POST /api/me/deletion", refuseTokens(s.RequireUser(http.HandlerFunc(s.handleDeleteOwn)).ServeHTTP))
	mux.Handle("POST /api/workspaces/{workspace}/members/{user}/deletion",
		refuseTokens(s.RequireAdmin(http.HandlerFunc(s.handleDeleteMember)).ServeHTTP))
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

// DeletionStatus is the reply of GET /api/me/deletion: what deleting the caller's account takes.
type DeletionStatus struct {
	// GraceDays is how long the account is kept, disabled, before it is purged.
	GraceDays int `json:"grace_days"`
	// Method is how the caller proves it is them: "password" (and TwoFactor, if they have it on),
	// or "fresh_login" for an account with no password, which is a sign-in made within the last
	// minutes (Fresh).
	Method    string `json:"method"`
	TwoFactor bool   `json:"two_factor"`
	Fresh     bool   `json:"fresh"`
	// Blockers are the workspaces where the caller is the only admin in use: while there are
	// any, deleting is refused.
	Blockers []DeletionBlocker `json:"blockers" apitypes:"nonnull"`
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

// freshSession reports whether the session token names a live session of userID made within
// freshLogin.
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

// DeletionReply is the answer to a request to delete an account.
type DeletionReply struct {
	// PurgeAt is when the account is purged, unless an operator cancels it before.
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
		purgeAt, err = s.requestDeletion(r.Context(), tx, u.ID, 0)
		return err
	})
	if !ok {
		return
	}
	slog.InfoContext(r.Context(), "an account was deleted by its person; it is purged after the grace period", "user_id", u.ID)
	s.endLive(u.ID, "")
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusAccepted, DeletionReply{PurgeAt: stamp(purgeAt)})
}

// ownReauth runs act in a transaction, after the caller proves it is them: their password, and
// their two-factor code if they have two-factor login on, or, for an account with no password, a
// login made within freshLogin. It answers any failure and reports whether act ran. A wrong
// password is a failed login to the limits, and so is a wrong code.
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
		// The password that was checked must still be the user's: a change or reset meanwhile
		// ended this session, and the request doesn't outlive it.
		if err := s.passwordUnchanged(ctx, tx, u.ID, verified); err != nil {
			return err
		}
		// Read here, in the transaction that holds the write lock, as the code is spent.
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

// deletionFailed answers err, if there is one, and reports whether there was none.
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
	case errors.Is(err, errDeletionOwn):
		writeError(w, http.StatusBadRequest, apierr.CodeDeletionOwn, err.Error())
	case errors.Is(err, errDeletionElsewhere):
		writeError(w, http.StatusForbidden, apierr.CodeDeletionElsewhere, err.Error())
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

func (s *Service) handleDeleteMember(w http.ResponseWriter, r *http.Request) {
	m, _ := MembershipFrom(r.Context())
	admin, _ := UserFrom(r.Context())
	userID, ok := pathID(w, r, "user", apierr.CodeMemberNotFound, "member not found")
	if !ok {
		return
	}
	if !readEmptyBody(w, r) {
		return
	}
	var purgeAt time.Time
	err := s.write(r.Context(), func(tx *sql.Tx) error {
		if _, err := roleIn(r.Context(), tx, m.WorkspaceID, userID); err != nil {
			return err
		}
		if userID == admin.ID {
			return errDeletionOwn
		}
		if all, err := administersAll(r.Context(), tx, admin.ID, userID); err != nil {
			return err
		} else if !all {
			return errDeletionElsewhere
		}
		// Read in the transaction, which holds the write lock: an admin disabled meanwhile deletes
		// nothing.
		if off, err := s.isDisabled(r.Context(), tx, admin.ID); err != nil {
			return err
		} else if off {
			return errAccountDisabled
		}
		var err error
		purgeAt, err = s.requestDeletion(r.Context(), tx, userID, admin.ID)
		return err
	})
	if !s.deletionFailed(w, r, err) {
		return
	}
	slog.InfoContext(r.Context(), "a member's account was deleted by an admin; it is purged after the grace period",
		"user_id", userID, "by", admin.ID, "workspace_id", m.WorkspaceID)
	s.endLive(userID, "")
	writeJSON(w, http.StatusAccepted, DeletionReply{PurgeAt: stamp(purgeAt)})
}

// requestDeletion switches userID's account off and records its deletion, within tx: by is the admin
// who asks, or 0 for the person themselves. It fails with errDeletionPending if the account is
// waiting already, and with a *blockedError if it would leave a workspace with no admin in use.
// Its sessions, reset links and half-done logins end; what it owns is kept until the purge.
func (s *Service) requestDeletion(ctx context.Context, tx *sql.Tx, userID, by int64) (time.Time, error) {
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
	var requestedBy any
	if by != 0 {
		requestedBy = by
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO account_deletions (user_id, requested_at, purge_at, requested_by, by_self, was_disabled)
		VALUES (?, ?, ?, ?, ?, ?)`, userID, stamp(now), stamp(purgeAt), requestedBy, by == 0, disabled); err != nil {
		return time.Time{}, err
	}
	if !disabled {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET disabled_at = ?, disabled_by = ? WHERE id = ?`,
			stamp(now), requestedBy, userID); err != nil {
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
	actor := by
	if actor == 0 {
		actor = userID
	}
	for _, ws := range workspaces.all {
		if err := audit.Record(ctx, tx, audit.Event{Workspace: ws, Actor: actor, Action: audit.AccountDeletionRequested,
			TargetType: audit.TargetUser, Target: userID,
			Details: audit.Details{"by_self": by == 0, "purge_days": int64(s.graceOf() / (24 * time.Hour)), "logins_ended": sessions}}); err != nil {
			return time.Time{}, err
		}
	}
	return purgeAt, nil
}

// userWorkspaces are the workspaces a user belongs to, and the one a job about them is filed
// under (the setup workspace's if they are in it, else their oldest; any workspace for one that is
// in none, since a job has to be in one).
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
	// Nothing about the person's workspaces is recorded, so the event is the server's.
	out.all = []int64{0}
	err = tx.QueryRowContext(ctx, `SELECT id FROM workspaces ORDER BY is_setup DESC, id LIMIT 1`).Scan(&out.home)
	return out, err
}

// PendingDeletion is an account waiting to be deleted, as the operator sees it.
type PendingDeletion struct {
	User        User   `json:"user"`
	RequestedAt string `json:"requested_at"`
	PurgeAt     string `json:"purge_at"`
	// Purging says the purge has begun, so the deletion can't be cancelled any more.
	Purging bool `json:"purging"`
	// BySelf says the person deleted their own account; otherwise an admin did.
	BySelf bool `json:"by_self"`
}

func (s *Service) handlePendingDeletions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT u.id, u.email, u.name, d.requested_at, d.purge_at, d.purge_started_at IS NOT NULL, d.by_self
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
		if err := rows.Scan(&p.User.ID, &p.User.Email, &p.User.Name, &p.RequestedAt, &p.PurgeAt, &p.Purging, &p.BySelf); err != nil {
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
	case err != nil:
		serverError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "an account's deletion was cancelled", "user_id", userID, "by", operator.ID)
	w.WriteHeader(http.StatusNoContent)
}

// cancelDeletion ends the wait of userID's account to be deleted, within tx: the account is
// switched on again, unless it was disabled before it was deleted, and its purge is removed. Its
// logins ended with the deletion, so the person signs in again.
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

// isDeleting reports whether userID's account is waiting to be deleted, reading through q.
func isDeleting(ctx context.Context, q querier, userID int64) (bool, error) {
	var pending bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM account_deletions WHERE user_id = ?)`, userID).Scan(&pending)
	return pending, err
}

// rehomePurge files the purge of userID again, if their account is waiting to be deleted: the
// scheduler drops a person's jobs of a workspace when they leave it (or are removed), and the
// purge must outlive that. It runs in the transaction that removes the membership.
func (s *Service) rehomePurge(ctx context.Context, tx *sql.Tx, userID int64) error {
	if s.deletions == nil {
		return nil
	}
	var purgeAt string
	err := tx.QueryRowContext(ctx, `SELECT purge_at FROM account_deletions WHERE user_id = ? AND purge_started_at IS NULL`, userID).Scan(&purgeAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	at, err := time.Parse(timeFormat, purgeAt)
	if err != nil {
		return err
	}
	workspaces, err := workspaceIDs(ctx, tx, userID)
	if err != nil {
		return err
	}
	return s.deletions.SchedulePurge(ctx, tx, userID, workspaces.home, at)
}
