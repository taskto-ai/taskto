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

// Your own sessions (docs/api.md#your-sessions): the browsers and devices logged in as you, to
// see and to sign out. The routes act on the caller only, as a session of the browser's own, and
// take no access token. Nothing here returns a session's token, which the database doesn't keep
// either, only its hash; a session is named by its public ID, which is no way into it and is
// never reused (the row's integer ID is, once its row is deleted).

var (
	errSessionCurrent = errors.New("this is the session you are using: sign out instead")
	// errSessionGone is for a request whose own login has ended since the middleware let it
	// through: it acts for nobody now.
	errSessionGone = errors.New("the session making the request has ended")
)

// Session is one of a user's logins, without its token.
type Session struct {
	ID string `json:"id"`
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
	sessions, err := s.sessionsOf(r.Context(), u.ID, hashToken(s.sessionToken(r)))
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]Session{"sessions": sessions})
}

func (s *Service) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	id := r.PathValue("session")
	if !validSessionPublicID(id) {
		writeError(w, http.StatusNotFound, apierr.CodeSessionNotFound, "session not found")
		return
	}
	err := s.revokeSession(r.Context(), u.ID, id, s.sessionToken(r))
	switch {
	case errors.Is(err, errSessionGone):
		writeError(w, http.StatusUnauthorized, apierr.CodeNotLoggedIn, "log in first")
		return
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
	slog.InfoContext(r.Context(), "a session was signed out", "user_id", u.ID, "session", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleRevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	n, err := s.revokeOtherSessions(r.Context(), u.ID, s.sessionToken(r))
	switch {
	case errors.Is(err, errSessionGone):
		writeError(w, http.StatusUnauthorized, apierr.CodeNotLoggedIn, "log in first")
		return
	case err != nil:
		serverError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "the other sessions were signed out", "user_id", u.ID, "sessions", n)
	writeJSON(w, http.StatusOK, map[string]int64{"revoked": n})
}

// sessionsOf lists userID's sessions that haven't expired, the current one (whose token hashes
// to currentHash) first and then the most recently used. Only userID's own are ever read.
func (s *Service) sessionsOf(ctx context.Context, userID int64, currentHash string) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT public_id, browser, os, created_at, COALESCE(last_seen_at, created_at), expires_at,
			token_hash = ?
		FROM sessions
		WHERE user_id = ? AND expires_at > ?
		ORDER BY token_hash = ? DESC, COALESCE(last_seen_at, created_at) DESC, id DESC`,
		currentHash, userID, stamp(s.now()), currentHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Session{}
	for rows.Next() {
		var x Session
		if err := rows.Scan(&x.ID, &x.Browser, &x.OS, &x.CreatedAt, &x.LastSeenAt, &x.ExpiresAt, &x.Current); err != nil {
			return nil, err
		}
		all = append(all, x)
	}
	return all, rows.Err()
}

// callerSession finds, in tx, the live session the request's token names, and errSessionGone if
// it has expired or been ended since the middleware looked: whoever asks to end sessions must
// still be logged in at the time they are ended, or a login made since (after a password reset,
// say) would go with the rest. The check is in the transaction that deletes, which holds the
// write lock.
func (s *Service) callerSession(ctx context.Context, tx *sql.Tx, userID int64, token string) (publicID string, err error) {
	if token == "" {
		return "", errSessionGone
	}
	err = tx.QueryRowContext(ctx, `SELECT public_id FROM sessions WHERE token_hash = ? AND user_id = ? AND expires_at > ?`,
		hashToken(token), userID, stamp(s.now())).Scan(&publicID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errSessionGone
	}
	return publicID, err
}

// revokeSession ends userID's session publicID, for the caller whose session token is token:
// errNotFound if it isn't theirs (or is gone), errSessionCurrent for the one asking, which
// logging out is for, and errSessionGone if the caller's own login has ended.
func (s *Service) revokeSession(ctx context.Context, userID int64, publicID, token string) error {
	var endedHash string
	err := s.write(ctx, func(tx *sql.Tx) error {
		current, err := s.callerSession(ctx, tx, userID, token)
		if err != nil {
			return err
		}
		if publicID == current {
			return errSessionCurrent
		}
		// The same test as sessionUser: one that has expired is no login to end, whether or not its
		// row has been swept yet.
		err = tx.QueryRowContext(ctx, `DELETE FROM sessions WHERE public_id = ? AND user_id = ? AND expires_at > ? RETURNING token_hash`,
			publicID, userID, stamp(s.now())).Scan(&endedHash)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Actor: userID, Action: audit.SessionRevoked,
			TargetType: audit.TargetUser, Target: userID})
	})
	if err == nil {
		s.endLiveHashes(userID, endedHash) // its open connections go with it
	}
	return err
}

// revokeOtherSessions ends every session of userID but the caller's, whose token is token,
// returning how many it ended, or errSessionGone, ending nothing, if the caller's own login has
// ended.
func (s *Service) revokeOtherSessions(ctx context.Context, userID int64, token string) (int64, error) {
	var ended []string
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := s.callerSession(ctx, tx, userID, token); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash <> ? AND expires_at > ? RETURNING token_hash`,
			userID, hashToken(token), stamp(s.now()))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				return err
			}
			ended = append(ended, h)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(ended) == 0 {
			return nil // none ended, nothing to record
		}
		return audit.Record(ctx, tx, audit.Event{Actor: userID, Action: audit.OtherSessionsRevoked,
			TargetType: audit.TargetUser, Target: userID, Details: audit.Details{"logins_ended": len(ended)}})
	})
	if err != nil {
		return 0, err
	}
	s.endLiveHashes(userID, ended...)
	return int64(len(ended)), nil
}
