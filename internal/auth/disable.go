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

var (
	errDisableOwn       = errors.New("you can't disable your own account")
	errDisableElsewhere = errors.New("this member also belongs to a workspace you aren't an admin of: only someone who administers all of their workspaces can disable or enable them")
)

func (s *Service) disableRoutes(mux *http.ServeMux) {
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
		case errors.Is(err, errDeletionPending):
			writeError(w, http.StatusConflict, apierr.CodeDeletionPending, err.Error())
			return
		case errors.Is(err, errDisableElsewhere):
			writeError(w, http.StatusForbidden, apierr.CodeDisableElsewhere, err.Error())
			return
		case errors.Is(err, errAccountDisabled):
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

var ErrAccountDisabled = errAccountDisabled

func EnsureActorEnabled(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) error {
	actor := audit.ActorFrom(ctx)
	if actor == 0 {
		return nil
	}
	var off bool
	err := q.QueryRowContext(ctx, `SELECT disabled_at IS NOT NULL FROM users WHERE id = ?`, actor).Scan(&off)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	case off:
		return errAccountDisabled
	}
	return nil
}

func WriteAccountDisabled(w http.ResponseWriter) { accountDisabled(w) }

func (s *Service) isDisabled(ctx context.Context, q querier, userID int64) (bool, error) {
	var disabled bool
	err := q.QueryRowContext(ctx, `SELECT disabled_at IS NOT NULL FROM users WHERE id = ?`, userID).Scan(&disabled)
	return disabled, err
}

func (s *Service) setDisabled(ctx context.Context, workspaceID, by, userID int64, disable bool) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := roleIn(ctx, tx, workspaceID, userID); err != nil {
			return err
		}
		if userID == by {
			return errDisableOwn
		}
		if pending, err := isDeleting(ctx, tx, userID); err != nil {
			return err
		} else if pending {
			return errDeletionPending
		}
		if all, err := administersAll(ctx, tx, by, userID); err != nil {
			return err
		} else if !all {
			return errDisableElsewhere
		}
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
			for _, h := range s.enabledHooks {
				if err := h(ctx, tx, userID); err != nil {
					return err
				}
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
		if err := s.endSignIns(ctx, tx, userID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Actor: by, Action: audit.MemberDisabled,
			TargetType: audit.TargetUser, Target: userID, Details: audit.Details{"logins_ended": sessions}})
	})
	if err == nil && disable {
		s.endLive(userID, "")
	}
	return err
}
