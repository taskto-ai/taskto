// Package auth is accounts and access: users, workspaces and their members, invites,
// password logins with session cookies, and the middleware that puts the caller on the
// request.
//
// One person or a company use the same model: a solo install is a workspace with one
// member, and a team is the same workspace after inviting people. A user can belong to
// several workspaces; picking one is picking its ID in the URL.
//
// Routes that act on a workspace go under /api/workspaces/{workspace}/ and wrap their
// handler in RequireMember or RequireAdmin; they then find the caller with UserFrom and
// MembershipFrom. CSRF protects every state-changing /api route, cookie-authenticated or
// not, so new routes are covered without doing anything.
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
	"sync/atomic"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

// Options are the account system's settings. Zero values take the defaults.
type Options struct {
	// InsecureCookies drops the Secure attribute from the session cookie, for plain-http
	// development on localhost.
	InsecureCookies bool
	// SessionTTL is how long a login lasts. Default 30 days.
	SessionTTL time.Duration
	// InviteTTL is how long an invite can be accepted. Default 7 days.
	InviteTTL time.Duration
	// ResetTTL is how long a password reset link works. Default 24 hours.
	ResetTTL time.Duration
	// HashParams are the argon2id costs for new password hashes. Default DefaultParams.
	HashParams Params
	// SetupToken is the token POST /api/setup requires (see NewSetupToken). Empty means
	// first-run setup is refused: the server makes one only while no user exists.
	SetupToken string
	// TrustedProxies are the reverse proxies whose X-Forwarded-For is believed, for the
	// client address the login limits use. Empty means none: the client is the direct peer.
	TrustedProxies []netip.Prefix
}

// Login limits, in failed attempts per window; a successful login counts against none.
//
//   - per client address: stops one client guessing across many accounts;
//   - per email and address: stops one client guessing one account, without letting it
//     lock that account for anyone else;
//   - per email: a higher ceiling that slows guessing spread over many addresses. Locking
//     someone out now takes their own address, or this many attempts from many addresses.
//
// Every limit is keyed by the normalized email as typed, the same way whether an account
// exists or not, so the answers never reveal which addresses have one.
const (
	ipAttempts           = 30
	accountAndIPAttempts = 10
	accountAttempts      = 100
	attemptWindow        = 15 * time.Minute
)

// Service holds the account system's state; its methods are the HTTP handlers and
// middleware.
type Service struct {
	db     *sql.DB
	opts   Options
	now    func() time.Time
	hasher *hasher

	// setupHash is the SHA-256 of Options.SetupToken, nil without one; setupDone is set
	// once setup has succeeded, so the token is unusable even before the server restarts.
	setupHash []byte
	setupDone atomic.Bool

	perIP        *limiter
	perAccountIP *limiter
	perAccount   *limiter
}

// New returns the account system backed by sqlDB, which must be migrated.
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
	s.perIP = newLimiter(ipAttempts, attemptWindow, now)
	s.perAccountIP = newLimiter(accountAndIPAttempts, attemptWindow, now)
	s.perAccount = newLimiter(accountAttempts, attemptWindow, now)
	return s
}

// cookieName is the session cookie's name. With Secure, the __Host- prefix makes the
// browser refuse the cookie unless it is also host-only with Path=/, so a sibling
// subdomain can't set or shadow it.
func (s *Service) cookieName() string {
	if s.opts.InsecureCookies {
		return "taskto_session"
	}
	return "__Host-taskto_session"
}

func (s *Service) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, s.sessionCookie(token, int(s.opts.SessionTTL/time.Second)))
}

func (s *Service) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, s.sessionCookie("", -1))
}

func (s *Service) sessionCookie(value string, maxAge int) *http.Cookie {
	// #nosec G124 -- Secure is off only when the operator sets insecure_cookies, for
	// plain-http development; HttpOnly and SameSite are always set.
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

// sessionToken returns the session token the request carries, or "".
func (s *Service) sessionToken(r *http.Request) string {
	c, err := r.Cookie(s.cookieName())
	if err != nil {
		return ""
	}
	return c.Value
}

// currentUser returns the user the request's session belongs to, with ok false if it has
// no valid session.
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

// SessionID returns the database ID of the login the request carries, for what is bound to a
// login and goes with it (a device's push subscription): ok is false for a request with no valid
// session, such as one made with an access token.
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

// UserFrom returns the user RequireUser, RequireMember or RequireAdmin put on ctx.
func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey{}).(User)
	return u, ok
}

// MembershipFrom returns the caller's membership of the workspace in the URL, put on ctx
// by RequireMember or RequireAdmin.
func MembershipFrom(ctx context.Context) (Membership, bool) {
	m, ok := ctx.Value(membershipKey{}).(Membership)
	return m, ok
}

// authenticate returns the request's user, or answers 401 (or 500) and returns false. A
// request carrying an access token is authenticated by the token alone, on routes that
// accept one (see AcceptTokens), and refused with 403 on the rest; t is then the token.
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
		if s.sessionToken(r) != "" {
			s.clearSessionCookie(w)
		}
		writeError(w, http.StatusUnauthorized, apierr.CodeNotLoggedIn, "log in first")
		return User{}, nil, false
	}
	return u, nil, true
}

// RequireUser lets through requests with a valid session, with the user on the context
// (see UserFrom), and answers 401 to the rest. It takes no access tokens: they act in one
// workspace, and the routes it guards are about the whole account.
func (s *Service) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	})
}

// RequireMember is for routes with a {workspace} path value: it lets through members of
// that workspace, with the user and the membership on the context (see MembershipFrom).
// It answers 401 without a session, and 404 to anyone else, so outsiders can't tell
// which workspaces exist. Wrapped in AcceptTokens, it also lets through access tokens of
// that workspace (see TokenFrom), whose user is still a member.
func (s *Service) RequireMember(next http.Handler) http.Handler {
	return s.requireRole(next, false)
}

// RequireAdmin is RequireMember for admins only; it answers 403 to other members.
func (s *Service) RequireAdmin(next http.Handler) http.Handler {
	return s.requireRole(next, true)
}

func (s *Service) requireRole(next http.Handler, admin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, t, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("workspace"), 10, 64)
		if err != nil || (t != nil && t.WorkspaceID != id) {
			// A token reaches its own workspace only.
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
		ctx := context.WithValue(r.Context(), userKey{}, u)
		ctx = context.WithValue(ctx, membershipKey{}, m)
		if t != nil {
			ctx = context.WithValue(ctx, tokenKey{}, *t)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// serverError logs err and answers 500 without details, which can name server paths.
func serverError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the client went away
	}
	slog.Error("auth: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	apierr.Internal(w)
}
