package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/mail"
	"taskto.ai/taskto/internal/ratelimit"
	"taskto.ai/taskto/internal/serverkey"
)

type Options struct {
	InsecureCookies      bool
	SessionTTL           time.Duration
	InviteTTL            time.Duration
	ResetTTL             time.Duration
	AccountDeletionGrace time.Duration
	HashParams           Params
	SetupToken           string
	TrustedProxies       []netip.Prefix
	SSO                  []SSOProvider
	PublicURL            string
	DisablePasswordLogin bool
	WorkspaceCreation    string
	Key                  *serverkey.Key
	Mail                 *mail.Service
}

const (
	CreateByAdmins = "admins"
	CreateByAnyone = "anyone"
	CreateByNobody = "off"
)

const (
	ipAttempts           = 30
	accountAndIPAttempts = 10
	accountAttempts      = 100
	attemptWindow        = 15 * time.Minute
)

type Service struct {
	db     *sql.DB
	opts   Options
	now    func() time.Time
	hasher *hasher

	setupHash []byte
	setupDone atomic.Bool

	perIP        *ratelimit.Limiter
	perAccountIP *ratelimit.Limiter
	perAccount   *ratelimit.Limiter

	codePerIP        *ratelimit.Limiter
	codePerAccountIP *ratelimit.Limiter
	codePerAccount   *ratelimit.Limiter

	key           *serverkey.Key
	resetPerIP    *ratelimit.Limiter
	resetPerEmail *ratelimit.Limiter
	resetStarts   *bucket
	mailSlots     chan struct{}
	mailWork      sync.WaitGroup
	workCtx       context.Context
	stopWork      context.CancelFunc

	invitePerAdmin     *ratelimit.Limiter
	invitePerRecipient *ratelimit.Limiter

	sso *sso

	live liveWatches

	hook func(point string)

	deletions DeletionScheduler

	memberHooks  []MemberHook
	zoneHooks    []TimeZoneHook
	enabledHooks []TimeZoneHook
}

func (s *Service) at(point string) {
	if s.hook != nil {
		s.hook(point)
	}
}

type MemberHook func(ctx context.Context, tx *sql.Tx, workspaceID, userID int64) error

func (s *Service) OnMemberAdded(h MemberHook) { s.memberHooks = append(s.memberHooks, h) }

func New(sqlDB *sql.DB, opts Options) *Service {
	if opts.SessionTTL == 0 {
		opts.SessionTTL = 30 * 24 * time.Hour
	}
	if opts.InviteTTL == 0 {
		opts.InviteTTL = 7 * 24 * time.Hour
	}
	if opts.ResetTTL == 0 {
		opts.ResetTTL = 24 * time.Hour
	}
	s := &Service{db: sqlDB, opts: opts, now: time.Now, hasher: newHasher(opts.HashParams)}
	if opts.SetupToken != "" {
		sum := sha256.Sum256([]byte(opts.SetupToken))
		s.setupHash = sum[:]
	}
	now := func() time.Time { return s.now() }
	s.perIP = ratelimit.New(ipAttempts, attemptWindow, now)
	s.perAccountIP = ratelimit.New(accountAndIPAttempts, attemptWindow, now)
	s.perAccount = ratelimit.New(accountAttempts, attemptWindow, now)
	s.codePerIP = ratelimit.New(codeIPAttempts, attemptWindow, now)
	s.codePerAccountIP = ratelimit.New(codeAccountAndIPAttempts, attemptWindow, now)
	s.codePerAccount = ratelimit.New(codeAccountAttempts, attemptWindow, now)
	s.key = opts.Key
	s.resetPerIP = ratelimit.New(resetIPAttempts, resetWindow, now)
	s.resetPerEmail = ratelimit.New(resetEmailAttempts, resetWindow, now)
	s.resetStarts = newBucket(resetStartBurst, resetStartEvery, now)
	s.mailSlots = make(chan struct{}, mailConcurrency)
	s.workCtx, s.stopWork = context.WithCancel(context.Background())
	s.invitePerAdmin = ratelimit.New(inviteAdminAttempts, inviteWindow, now)
	s.invitePerRecipient = ratelimit.New(inviteRecipientAttempts, inviteWindow, now)
	s.sso = newSSO(opts.SSO, now)
	return s
}

func (s *Service) cookieName() string {
	if s.opts.InsecureCookies {
		return "taskto_session"
	}
	return "__Host-taskto_session"
}

func (s *Service) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, s.sessionCookie(token, int(s.opts.SessionTTL/time.Second)))
}

func (s *Service) sessionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     s.cookieName(),
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   !s.opts.InsecureCookies,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

func (s *Service) sessionToken(r *http.Request) string {
	c, err := r.Cookie(s.cookieName())
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Service) currentUser(r *http.Request) (u User, ok bool, err error) {
	token := s.sessionToken(r)
	if token == "" {
		return User{}, false, nil
	}
	u, err = s.sessionUser(r.Context(), token)
	switch {
	case errors.Is(err, errNotFound):
		return User{}, false, nil
	case err != nil:
		return User{}, false, err
	}
	return u, true, nil
}

func (s *Service) SessionID(r *http.Request) (id int64, ok bool, err error) {
	token := s.sessionToken(r)
	if token == "" {
		return 0, false, nil
	}
	err = s.db.QueryRowContext(r.Context(), `SELECT id FROM sessions WHERE token_hash = ? AND expires_at > ?`,
		hashToken(token), stamp(s.now())).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

type (
	userKey       struct{}
	membershipKey struct{}
)

func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey{}).(User)
	return u, ok
}

func MembershipFrom(ctx context.Context) (Membership, bool) {
	m, ok := ctx.Value(membershipKey{}).(Membership)
	return m, ok
}

func (s *Service) authenticate(w http.ResponseWriter, r *http.Request) (u User, t *Token, ok bool) {
	if raw, isToken := BearerToken(r); isToken {
		return s.tokenRequest(w, r, raw)
	}
	u, ok, err := s.currentUser(r)
	if err != nil {
		serverError(w, r, err)
		return User{}, nil, false
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, apierr.CodeNotLoggedIn, "log in first")
		return User{}, nil, false
	}
	return u, nil, true
}

func (s *Service) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		ctx := audit.WithActor(context.WithValue(r.Context(), userKey{}, u), u.ID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Service) RequireMember(next http.Handler) http.Handler {
	return s.requireRole(next, false)
}

func (s *Service) RequireAdmin(next http.Handler) http.Handler {
	return s.requireRole(next, true)
}

func (s *Service) RequireOperator(next http.Handler) http.Handler {
	return s.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFrom(r.Context())
		ok, err := s.IsOperator(r.Context(), u.ID)
		if err != nil {
			serverError(w, r, err)
			return
		}
		if !ok {
			writeError(w, http.StatusForbidden, apierr.CodeOperatorOnly, "only the server's operator can do this")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Service) IsOperator(ctx context.Context, userID int64) (bool, error) {
	return isOperator(ctx, s.db, userID)
}

func isOperator(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, userID int64) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM memberships m JOIN workspaces w ON w.id = m.workspace_id
		WHERE m.user_id = ? AND m.role = ? AND w.is_setup = 1)`, userID, RoleAdmin).Scan(&ok)
	return ok, err
}

func (s *Service) requireRole(next http.Handler, admin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, t, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("workspace"), 10, 64)
		if err != nil || (t != nil && t.WorkspaceID != id) {
			writeError(w, http.StatusNotFound, apierr.CodeWorkspaceNotFound, "workspace not found")
			return
		}
		m, err := s.membership(r.Context(), id, u.ID)
		if errors.Is(err, errNotFound) {
			writeError(w, http.StatusNotFound, apierr.CodeWorkspaceNotFound, "workspace not found")
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		if admin && m.Role != RoleAdmin {
			writeError(w, http.StatusForbidden, apierr.CodeAdminOnly, "only a workspace admin can do this")
			return
		}
		ctx := audit.WithActor(context.WithValue(r.Context(), userKey{}, u), u.ID)
		ctx = context.WithValue(ctx, membershipKey{}, m)
		if t != nil {
			ctx = context.WithValue(ctx, tokenKey{}, *t)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func serverError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	slog.Error("auth: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	apierr.Internal(w)
}
