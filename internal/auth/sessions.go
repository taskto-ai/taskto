package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"taskto.ai/taskto/internal/apierr"
)

// Your own sessions (docs/api.md#your-sessions): the browsers and devices logged in as you, to
// see and to sign out. The routes act on the caller only, as a session of the browser's own, and
// take no access token. Nothing here returns a session's token, which the database doesn't keep
// either, only its hash; a session is named by its ID, which is no way into it.

var errSessionCurrent = errors.New("this is the session you are using: sign out instead")

// Session is one of a user's logins, without its token.
type Session struct {
	ID int64 `json:"id"`
	// Browser and OS name the product the login was made with ("Firefox", "macOS"), or are ""
	// when the User-Agent didn't say. The web app puts them in a sentence of the user's language.
	Browser    string `json:"browser"`
	OS         string `json:"os"`
	CreatedAt  string `json:"created_at"`
	LastSeenAt string `json:"last_seen_at"`
	ExpiresAt  string `json:"expires_at"`
	// Current marks the session of the request that asked.
	Current bool `json:"current"`
}

func (s *Service) sessionRoutes(mux *http.ServeMux) {
	// RequireUser takes no access tokens; refuseTokens says so up front.
	mux.Handle("GET /api/me/sessions", refuseTokens(s.RequireUser(http.HandlerFunc(s.handleSessions)).ServeHTTP))
	mux.Handle("DELETE /api/me/sessions", refuseTokens(s.RequireUser(http.HandlerFunc(s.handleRevokeOtherSessions)).ServeHTTP))
	mux.Handle("DELETE /api/me/sessions/{session}", refuseTokens(s.RequireUser(http.HandlerFunc(s.handleRevokeSession)).ServeHTTP))
}

func (s *Service) handleSessions(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	current, _, err := s.SessionID(r)
	if err != nil {
		serverError(w, r, err)
		return
	}
	sessions, err := s.sessionsOf(r.Context(), u.ID, current)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]Session{"sessions": sessions})
}

func (s *Service) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	id, ok := pathID(w, r, "session", apierr.CodeSessionNotFound, "session not found")
	if !ok {
		return
	}
	current, _, err := s.SessionID(r)
	if err != nil {
		serverError(w, r, err)
		return
	}
	err = s.revokeSession(r.Context(), u.ID, id, current)
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, apierr.CodeSessionNotFound, "session not found")
		return
	case errors.Is(err, errSessionCurrent):
		writeError(w, http.StatusBadRequest, apierr.CodeSessionCurrent, errSessionCurrent.Error())
		return
	case err != nil:
		serverError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "a session was signed out", "user_id", u.ID, "session_id", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleRevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	current, _, err := s.SessionID(r)
	if err != nil {
		serverError(w, r, err)
		return
	}
	n, err := s.revokeOtherSessions(r.Context(), u.ID, current)
	if err != nil {
		serverError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "the other sessions were signed out", "user_id", u.ID, "sessions", n)
	writeJSON(w, http.StatusOK, map[string]int64{"revoked": n})
}

// sessionsOf lists userID's sessions that haven't expired, the current one first and then the
// most recently used. Only userID's own are ever read.
func (s *Service) sessionsOf(ctx context.Context, userID, current int64) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, browser, os, created_at, COALESCE(last_seen_at, created_at), expires_at
		FROM sessions
		WHERE user_id = ? AND expires_at > ?
		ORDER BY (id = ?) DESC, COALESCE(last_seen_at, created_at) DESC, id DESC`,
		userID, stamp(s.now()), current)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Session{}
	for rows.Next() {
		var x Session
		if err := rows.Scan(&x.ID, &x.Browser, &x.OS, &x.CreatedAt, &x.LastSeenAt, &x.ExpiresAt); err != nil {
			return nil, err
		}
		x.Current = x.ID == current
		all = append(all, x)
	}
	return all, rows.Err()
}

// revokeSession ends userID's session id: errNotFound if it isn't theirs (or is gone), and
// errSessionCurrent for the one asking, which logging out is for.
func (s *Service) revokeSession(ctx context.Context, userID, id, current int64) error {
	if id == current {
		// Checked against the caller's own sessions too, so another user's ID never gets this
		// answer.
		var own bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE id = ? AND user_id = ?)`,
			id, userID).Scan(&own); err != nil {
			return err
		}
		if own {
			return errSessionCurrent
		}
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	return mustAffect(res, errNotFound)
}

// revokeOtherSessions ends every session of userID but current, returning how many it ended.
func (s *Service) revokeOtherSessions(ctx context.Context, userID, current int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id <> ? AND expires_at > ?`,
		userID, current, stamp(s.now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
