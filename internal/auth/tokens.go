package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
)

const AccessTokenPrefix = "ttp_"

const (
	maxTokens       = 50
	tokenTouchEvery = time.Minute
)

type Token struct {
	ID          int64   `json:"id"`
	WorkspaceID int64   `json:"workspace_id"`
	Name        string  `json:"name"`
	CreatedAt   string  `json:"created_at"`
	ExpiresAt   *string `json:"expires_at"`
	LastUsedAt  *string `json:"last_used_at"`
}

func TokenAgent(name string) string { return name + " (access token)" }

var (
	ErrTokenInvalid  = errors.New("the access token is unknown, revoked or expired")
	errTooManyTokens = fmt.Errorf("a member can have at most %d access tokens in a workspace: revoke one first", maxTokens)
)

const errTokensRefused = "access tokens can't be used here: this needs a login in the browser"

type (
	tokenKey         struct{}
	acceptsTokensKey struct{}
)

func TokenFrom(ctx context.Context) (Token, bool) {
	t, ok := ctx.Value(tokenKey{}).(Token)
	return t, ok
}

func AcceptTokens(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), acceptsTokensKey{}, true)))
	})
}

func BearerToken(r *http.Request) (string, bool) {
	scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, strings.HasPrefix(value, AccessTokenPrefix)
}

func refuseTokens(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := BearerToken(r); ok {
			writeError(w, http.StatusForbidden, apierr.CodeTokenRefused, errTokensRefused)
			return
		}
		next(w, r)
	})
}

func (s *Service) TokenUser(ctx context.Context, raw string) (User, Token, error) {
	if !strings.HasPrefix(raw, AccessTokenPrefix) || len(raw) > 128 {
		return User{}, Token{}, ErrTokenInvalid
	}
	now := s.now()
	var u User
	var t Token
	err := s.db.QueryRowContext(ctx, `
		SELECT t.id, t.workspace_id, t.name, t.created_at, t.expires_at, t.last_used_at, u.id, u.email, u.name
		FROM access_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ? AND (t.expires_at IS NULL OR t.expires_at > ?)
		  AND u.disabled_at IS NULL
		  AND EXISTS (SELECT 1 FROM memberships m WHERE m.workspace_id = t.workspace_id AND m.user_id = t.user_id)`,
		hashToken(raw), stamp(now)).
		Scan(&t.ID, &t.WorkspaceID, &t.Name, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt, &u.ID, &u.Email, &u.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, Token{}, ErrTokenInvalid
	}
	if err != nil {
		return User{}, Token{}, err
	}
	if t.LastUsedAt == nil || *t.LastUsedAt <= stamp(now.Add(-tokenTouchEvery)) {
		used, err := s.touchToken(ctx, u.ID, t, hashToken(raw), now)
		if err != nil {
			slog.WarnContext(ctx, "auth: recording an access token's use", "token_id", t.ID, "err", err)
		} else if used != "" {
			t.LastUsedAt = &used
		}
	}
	return u, t, nil
}

func (s *Service) touchToken(ctx context.Context, user int64, t Token, hash string, now time.Time) (string, error) {
	used := stamp(now)
	var advanced bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE access_tokens SET last_used_at = ?
			WHERE id = ? AND token_hash = ? AND user_id = ? AND workspace_id = ?
			  AND (last_used_at IS NULL OR last_used_at <= ?)`,
			used, t.ID, hash, user, t.WorkspaceID, stamp(now.Add(-tokenTouchEvery)))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		advanced = true
		return audit.Record(ctx, tx, audit.Event{Workspace: t.WorkspaceID, Actor: user, Action: audit.AccessTokenUsed,
			TargetType: audit.TargetAccessToken, Target: t.ID, TargetCreated: t.CreatedAt})
	})
	if err != nil || !advanced {
		return "", err
	}
	return used, nil
}

func (s *Service) tokenRequest(w http.ResponseWriter, r *http.Request, raw string) (User, *Token, bool) {
	if accepts, _ := r.Context().Value(acceptsTokensKey{}).(bool); !accepts {
		writeError(w, http.StatusForbidden, apierr.CodeTokenRefused, errTokensRefused)
		return User{}, nil, false
	}
	u, t, err := s.TokenUser(r.Context(), raw)
	if errors.Is(err, ErrTokenInvalid) {
		writeError(w, http.StatusUnauthorized, apierr.CodeTokenInvalid, ErrTokenInvalid.Error())
		return User{}, nil, false
	}
	if err != nil {
		serverError(w, r, err)
		return User{}, nil, false
	}
	return u, &t, true
}

func (s *Service) handleTokens(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	m, _ := MembershipFrom(r.Context())
	tokens, err := s.tokensOf(r.Context(), m.WorkspaceID, u.ID)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]Token{"tokens": tokens})
}

func (s *Service) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	m, _ := MembershipFrom(r.Context())
	var req struct {
		Name      string  `json:"name"`
		ExpiresAt *string `json:"expires_at"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if !validName(name) {
		nameError(w)
		return
	}
	var expires *string
	if req.ExpiresAt != nil {
		at, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil || !at.After(s.now()) {
			writeError(w, http.StatusBadRequest, apierr.CodeTokenExpiryInvalid,
				"expires_at must be a time in the future, in RFC 3339 (like 2027-01-31T00:00:00Z)")
			return
		}
		e := stamp(at)
		expires = &e
	}
	t, token, err := s.createToken(r.Context(), m.WorkspaceID, u.ID, name, expires)
	if errors.Is(err, errTooManyTokens) {
		writeError(w, http.StatusConflict, apierr.CodeTooManyTokens, err.Error(), apierr.Params{"max": maxTokens})
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, TokenCreated{Token: t, Secret: token})
}

type TokenCreated struct {
	Token
	Secret string `json:"token"`
}

func (s *Service) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	m, _ := MembershipFrom(r.Context())
	id, ok := pathID(w, r, "token", apierr.CodeTokenNotFound, "token not found")
	if !ok {
		return
	}
	err := s.write(r.Context(), func(tx *sql.Tx) error {
		var created string
		err := tx.QueryRowContext(r.Context(), `DELETE FROM access_tokens WHERE id = ? AND workspace_id = ? AND user_id = ? RETURNING created_at`,
			id, m.WorkspaceID, u.ID).Scan(&created)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(r.Context(), tx, audit.Event{Workspace: m.WorkspaceID, Action: audit.AccessTokenRevoked,
			TargetType: audit.TargetAccessToken, Target: id, TargetCreated: created})
	})
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, apierr.CodeTokenNotFound, "token not found")
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) createToken(ctx context.Context, workspaceID, userID int64, name string, expires *string) (Token, string, error) {
	t := Token{WorkspaceID: workspaceID, Name: name, CreatedAt: stamp(s.now()), ExpiresAt: expires}
	secret, _ := newToken()
	token := AccessTokenPrefix + secret
	err := s.write(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM access_tokens WHERE workspace_id = ? AND user_id = ?`,
			workspaceID, userID).Scan(&n); err != nil {
			return err
		}
		if n >= maxTokens {
			return errTooManyTokens
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO access_tokens (user_id, workspace_id, name, token_hash, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?)`, userID, workspaceID, name, hashToken(token), t.CreatedAt, expires)
		if err != nil {
			return err
		}
		if t.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Actor: userID, Action: audit.AccessTokenCreated,
			TargetType: audit.TargetAccessToken, Target: t.ID, TargetCreated: t.CreatedAt, Details: audit.Details{"expires": expires != nil}})
	})
	if err != nil {
		return Token{}, "", err
	}
	return t, token, nil
}

func (s *Service) tokensOf(ctx context.Context, workspaceID, userID int64) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, workspace_id, name, created_at, expires_at, last_used_at
		FROM access_tokens WHERE workspace_id = ? AND user_id = ? ORDER BY id`, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Token{}
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.WorkspaceID, &t.Name, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt); err != nil {
			return nil, err
		}
		all = append(all, t)
	}
	return all, rows.Err()
}
