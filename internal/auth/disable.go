package auth

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
)

// Disabling a member (docs/api.md#disabling-a-member). An admin switches an account off without
// removing it: the account is signed out everywhere and can't sign in again (password or single
// sign-on, or a reset link), its access tokens stop working, and its conversations' agents and
// CLI turns are refused from their next call. Nothing it owns goes: its memberships, its spaces'
// files, its boards and its tokens stay, so enabling it again restores its access as it was.
//
// An account belongs to every workspace its person is in, so the switch is the account's, not one
// workspace's, and the rule for who may flip it is the one for a password reset link: an admin of
// every workspace the account belongs to (administersAll), never their own account. Such an admin
// is an admin of each of them besides the target, and is in use themselves (a disabled account
// has no session to ask with, and setDisabled checks it again), so a workspace is never left
// without an admin in use by this. Changing a role or removing a member counts admins in use
// only, so they can't leave it that way either (checkNotLastAdmin).

var (
	errDisableOwn       = errors.New("you can't disable your own account")
	errDisableElsewhere = errors.New("this member also belongs to a workspace you aren't an admin of: only someone who administers all of their workspaces can disable or enable them")
)

func (s *Service) disableRoutes(mux *http.ServeMux) {
	// RequireAdmin takes no access tokens; refuseTokens says so up front.
	mux.Handle("POST /api/workspaces/{workspace}/members/{user}/disable",
		refuseTokens(s.RequireAdmin(s.disableHandler(true)).ServeHTTP))
	mux.Handle("POST /api/workspaces/{workspace}/members/{user}/enable",
		refuseTokens(s.RequireAdmin(s.disableHandler(false)).ServeHTTP))
}

func (s *Service) disableHandler(disable bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m, _ := MembershipFrom(r.Context())
		admin, _ := UserFrom(r.Context())
		userID, ok := pathID(w, r, "user", apierr.CodeMemberNotFound, "member not found")
		if !ok {
			return
		}
		if !readEmptyBody(w, r) {
			return
		}
		err := s.setDisabled(r.Context(), m.WorkspaceID, admin.ID, userID, disable)
		switch {
		case errors.Is(err, errNotFound):
			writeError(w, http.StatusNotFound, apierr.CodeMemberNotFound, "member not found")
			return
		case errors.Is(err, errDisableOwn):
			writeError(w, http.StatusBadRequest, apierr.CodeDisableOwn, err.Error())
			return
		case errors.Is(err, errDisableElsewhere):
			writeError(w, http.StatusForbidden, apierr.CodeDisableElsewhere, err.Error())
			return
		case errors.Is(err, errAccountDisabled):
			// The admin was disabled meanwhile, by another admin.
			accountDisabled(w)
			return
		case err != nil:
			serverError(w, r, err)
			return
		}
		slog.InfoContext(r.Context(), "member disabled or enabled", "user_id", userID, "by", admin.ID,
			"workspace_id", m.WorkspaceID, "disabled", disable)
		w.WriteHeader(http.StatusNoContent)
	})
}

// isDisabled reports whether userID's account is disabled, reading through q.
func (s *Service) isDisabled(ctx context.Context, q querier, userID int64) (bool, error) {
	var disabled bool
	err := q.QueryRowContext(ctx, `SELECT disabled_at IS NOT NULL FROM users WHERE id = ?`, userID).Scan(&disabled)
	return disabled, err
}

// setDisabled switches userID's account off or on, for the admin by acting in workspaceID, where
// userID must be a member. It fails with errNotFound if they aren't, errDisableOwn for the
// admin's own account, errDisableElsewhere if userID belongs to a workspace by doesn't
// administer, and errAccountDisabled if by was disabled meanwhile. Disabling ends the account's
// sessions, which takes their push subscriptions, and its reset links; one that is disabled
// already, or enabled, is left as it is and records nothing.
func (s *Service) setDisabled(ctx context.Context, workspaceID, by, userID int64, disable bool) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := roleIn(ctx, tx, workspaceID, userID); err != nil {
			return err
		}
		if userID == by {
			return errDisableOwn
		}
		if all, err := administersAll(ctx, tx, by, userID); err != nil {
			return err
		} else if !all {
			return errDisableElsewhere
		}
		// Read in the transaction, which holds the write lock: of two admins disabling each other
		// at once, the second finds itself disabled.
		if off, err := s.isDisabled(ctx, tx, by); err != nil {
			return err
		} else if off {
			return errAccountDisabled
		}
		if !disable {
			res, err := tx.ExecContext(ctx, `UPDATE users SET disabled_at = NULL, disabled_by = NULL
				WHERE id = ? AND disabled_at IS NOT NULL`, userID)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil || n == 0 {
				return err
			}
			return audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Actor: by, Action: audit.MemberEnabled,
				TargetType: audit.TargetUser, Target: userID})
		}
		res, err := tx.ExecContext(ctx, `UPDATE users SET disabled_at = ?, disabled_by = ?
			WHERE id = ? AND disabled_at IS NULL`, stamp(s.now()), by, userID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		ended, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
		if err != nil {
			return err
		}
		sessions, err := ended.RowsAffected()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM password_resets WHERE user_id = ?`, userID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Actor: by, Action: audit.MemberDisabled,
			TargetType: audit.TargetUser, Target: userID, Details: audit.Details{"logins_ended": sessions}})
	})
}
