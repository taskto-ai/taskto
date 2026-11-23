package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"taskto.ai/taskto/internal/apierr"
)

// Single sign-on through OpenID Connect (docs/sso.md).
//
// A login is the authorization-code flow with PKCE, a nonce and a state:
//
//  1. POST /api/sso/{provider}/start makes the state, nonce and PKCE verifier, keeps them in
//     memory under the state for ssoTTL, sets the state in a cookie, and answers with the
//     provider's authorization URL, which the web app opens.
//  2. The provider redirects the browser to GET /api/sso/{provider}/callback. The callback is
//     the one state-changing GET, since the provider's redirect can't be a POST. It is safe
//     because it acts only on a state that is in the browser's own cookie (so a forged link
//     can't complete a login in another browser) and in the server's memory, once (a replay
//     finds nothing), and the code it exchanges is checked by PKCE and the ID token by its
//     nonce. Whatever happens, it answers with a redirect: to the page the person was on, or
//     to /?sso_error=<code>.
//  3. The ID token is verified (signature, issuer, audience, expiry, nonce) and its subject
//     found in user_identities, or linked to the user with its verified email, or the account
//     is made from a pending invite. Otherwise the login is refused: nobody signs up by
//     signing in.
const (
	ssoTTL        = 10 * time.Minute
	ssoMaxPending = 10_000
	ssoTimeout    = 15 * time.Second
	maxNextLen    = 2048
	maxSubject    = 255
)

// sso is the single sign-on state of a Service.
type sso struct {
	providers []*ssoClient
	byID      map[string]*ssoClient
	// hc makes every call to a provider: discovery, keys, the token and user info requests.
	hc         *http.Client
	startLimit *limiter

	mu      sync.Mutex
	pending map[string]ssoPending
}

// ssoClient is a configured provider and, once it has answered, its discovered metadata.
type ssoClient struct {
	cfg SSOProvider

	mu       sync.Mutex
	provider *oidc.Provider
}

// ssoPending is what a login in progress needs back at the callback.
type ssoPending struct {
	provider, nonce, verifier, next string
	expires                         time.Time
}

func newSSO(providers []SSOProvider, now func() time.Time) *sso {
	x := &sso{
		byID:       map[string]*ssoClient{},
		hc:         &http.Client{Timeout: ssoTimeout},
		startLimit: newLimiter(ipAttempts, attemptWindow, now),
		pending:    map[string]ssoPending{},
	}
	for _, p := range providers {
		c := &ssoClient{cfg: p}
		x.providers = append(x.providers, c)
		x.byID[p.ID] = c
	}
	return x
}

// discover returns the provider's metadata, asking it on first use only: a provider that is
// down doesn't stop the server from starting, and is tried again on the next login.
func (c *ssoClient) discover(ctx context.Context, hc *http.Client) (*oidc.Provider, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.provider != nil {
		return c.provider, nil
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, hc), ssoTimeout)
	defer cancel()
	p, err := oidc.NewProvider(ctx, c.cfg.Issuer)
	if err != nil {
		return nil, err
	}
	c.provider = p
	return p, nil
}

func (s *Service) ssoRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/login/methods", refuseTokens(s.handleLoginMethods))
	mux.Handle("POST /api/sso/{provider}/start", refuseTokens(s.handleSSOStart))
	mux.Handle("GET /api/sso/{provider}/callback", refuseTokens(s.handleSSOCallback))
}

// handleLoginMethods says how people can sign in, for the login screen.
func (s *Service) handleLoginMethods(w http.ResponseWriter, _ *http.Request) {
	type method struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	list := []method{}
	for _, c := range s.sso.providers {
		list = append(list, method{c.cfg.ID, c.cfg.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"password": !s.opts.DisablePasswordLogin, "sso": list})
}

func (s *Service) ssoCookieName() string {
	if s.opts.InsecureCookies {
		return "taskto_sso"
	}
	return "__Host-taskto_sso"
}

func (s *Service) ssoCookie(value string, maxAge int) *http.Cookie {
	// #nosec G124 -- Secure is off only when the operator sets insecure_cookies, for
	// plain-http development; HttpOnly and SameSite are always set.
	return &http.Cookie{
		Name:     s.ssoCookieName(),
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   !s.opts.InsecureCookies,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

func (s *Service) oauthConfig(c *ssoClient, p *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.cfg.ClientID,
		ClientSecret: c.cfg.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  s.opts.PublicURL + "/api/sso/" + c.cfg.ID + "/callback",
		Scopes:       c.cfg.Scopes,
	}
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// safeNext returns next if it is a path on this server to go back to after the login, and
// "/" otherwise: it must start with a single "/", and have no backslash or control character,
// which some browsers treat as another "/", or an authority after all.
func safeNext(next string) string {
	if next == "" || len(next) > maxNextLen || next[0] != '/' || strings.HasPrefix(next, "//") ||
		strings.HasPrefix(next, `/\`) || !utf8.ValidString(next) {
		return "/"
	}
	for _, r := range next {
		if r == '\\' || unicode.IsControl(r) {
			return "/"
		}
	}
	return next
}

func (s *Service) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	c := s.sso.byID[r.PathValue("provider")]
	if c == nil {
		writeError(w, http.StatusNotFound, apierr.CodeSSOUnknownProvider, "unknown sign-in provider")
		return
	}
	var req struct {
		Next string `json:"next"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, &req) {
		return
	}
	if ok, wait := s.sso.startLimit.take(s.clientIP(r)); !ok {
		tooMany(w, wait)
		return
	}
	prov, err := c.discover(r.Context(), s.sso.hc)
	if err != nil {
		slog.WarnContext(r.Context(), "sso: provider discovery failed", "provider", c.cfg.ID, "err", err)
		writeError(w, http.StatusBadGateway, apierr.CodeSSOFailed, "the sign-in provider didn't answer")
		return
	}
	state, err := randomToken()
	if err != nil {
		serverError(w, r, err)
		return
	}
	nonce, err := randomToken()
	if err != nil {
		serverError(w, r, err)
		return
	}
	verifier := oauth2.GenerateVerifier()
	if !s.sso.put(s.now(), state, ssoPending{provider: c.cfg.ID, nonce: nonce, verifier: verifier,
		next: safeNext(req.Next), expires: s.now().Add(ssoTTL)}) {
		tooMany(w, time.Minute)
		return
	}
	http.SetCookie(w, s.ssoCookie(state, int(ssoTTL/time.Second)))
	writeJSON(w, http.StatusOK, map[string]string{
		"url": s.oauthConfig(c, prov).AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)),
	})
}

// put keeps a login in progress. It reports false when too many are waiting, after dropping
// the expired ones: the memory a client can use is bounded, whoever it is.
func (x *sso) put(now time.Time, state string, p ssoPending) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.pending) >= ssoMaxPending {
		for k, v := range x.pending {
			if !now.Before(v.expires) {
				delete(x.pending, k)
			}
		}
		if len(x.pending) >= ssoMaxPending {
			return false
		}
	}
	x.pending[state] = p
	return true
}

// take removes and returns the login in progress for state: it works once.
func (x *sso) take(now time.Time, state string) (ssoPending, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	p, ok := x.pending[state]
	delete(x.pending, state)
	return p, ok && now.Before(p.expires)
}

// ssoFailure redirects the browser to the login screen with the reason.
func ssoFailure(w http.ResponseWriter, code apierr.Code) {
	redirectSeeOther(w, "/?sso_error="+url.QueryEscape(string(code)))
}

// redirectSeeOther sends the browser to a path on this server. The Location is set as it is:
// http.Redirect would clean the path, which could change what next means.
func redirectSeeOther(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusSeeOther)
}

func (s *Service) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	// The cookie did its job whatever happens next.
	http.SetCookie(w, s.ssoCookie("", -1))
	c := s.sso.byID[r.PathValue("provider")]
	if c == nil {
		ssoFailure(w, apierr.CodeSSOUnknownProvider)
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	cookie, err := r.Cookie(s.ssoCookieName())
	if state == "" || err != nil || subtle.ConstantTimeCompare([]byte(state), []byte(cookie.Value)) != 1 {
		ssoFailure(w, apierr.CodeSSOExpired)
		return
	}
	pend, ok := s.sso.take(s.now(), state)
	if !ok || pend.provider != c.cfg.ID {
		ssoFailure(w, apierr.CodeSSOExpired)
		return
	}
	if e := q.Get("error"); e != "" {
		// The provider refused or the person declined; its error is a short code, not a secret.
		slog.InfoContext(r.Context(), "sso: the provider returned an error", "provider", c.cfg.ID, "error", truncate(e, 64))
		ssoFailure(w, apierr.CodeSSOFailed)
		return
	}
	id, err := s.ssoExchange(r.Context(), c, pend, q.Get("code"))
	if err != nil {
		slog.WarnContext(r.Context(), "sso: the sign-in failed", "provider", c.cfg.ID, "err", err)
		ssoFailure(w, apierr.CodeSSOFailed)
		return
	}
	u, token, err := s.ssoLogin(r.Context(), c, id)
	if err != nil {
		code := apierr.CodeSSOFailed
		switch {
		case errors.Is(err, errSSOUnverified):
			code = apierr.CodeSSOEmailUnverified
		case errors.Is(err, errSSONoAccount):
			code = apierr.CodeSSONoAccount
		case errors.Is(err, errSSOLinkedElsewhere):
			code = apierr.CodeSSOAccountLinkedElsewhere
		}
		if code == apierr.CodeSSOFailed {
			slog.ErrorContext(r.Context(), "sso: the login failed", "provider", c.cfg.ID, "err", err)
		} else {
			slog.InfoContext(r.Context(), "sso: the login was refused", "provider", c.cfg.ID, "reason", string(code))
		}
		ssoFailure(w, code)
		return
	}
	// As in handleLogin: the new session first, then the one the browser had.
	if old := s.sessionToken(r); old != "" {
		if err := s.deleteSession(r.Context(), old); err != nil {
			slog.WarnContext(r.Context(), "auth: deleting the replaced session", "err", err)
		}
	}
	slog.InfoContext(r.Context(), "sso login", "provider", c.cfg.ID, "user_id", u.ID)
	s.setSessionCookie(w, token)
	redirectSeeOther(w, pend.next)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return strings.ToValidUTF8(s[:n], "")
	}
	return s
}

// ssoIdentity is what the provider says about the person.
type ssoIdentity struct {
	subject, email, name string
	verified             bool
}

// flexBool reads a JSON boolean, or the string "true" some providers send instead.
type flexBool bool

func (b *flexBool) UnmarshalJSON(d []byte) error {
	*b = string(d) == "true" || string(d) == `"true"`
	return nil
}

// ssoExchange trades the authorization code for tokens, with the PKCE verifier, and verifies
// the ID token: signature, issuer, audience, expiry and the nonce of this login.
func (s *Service) ssoExchange(ctx context.Context, c *ssoClient, pend ssoPending, code string) (ssoIdentity, error) {
	if code == "" {
		return ssoIdentity{}, errors.New("no authorization code")
	}
	prov, err := c.discover(ctx, s.sso.hc)
	if err != nil {
		return ssoIdentity{}, fmt.Errorf("discovery: %w", err)
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, s.sso.hc), ssoTimeout)
	defer cancel()
	tok, err := s.oauthConfig(c, prov).Exchange(ctx, code, oauth2.VerifierOption(pend.verifier))
	if err != nil {
		// A RetrieveError carries the provider's whole response body: keep its code only.
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			return ssoIdentity{}, fmt.Errorf("token exchange: status %d, error %q", re.Response.StatusCode, truncate(re.ErrorCode, 64))
		}
		return ssoIdentity{}, fmt.Errorf("token exchange: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return ssoIdentity{}, errors.New("the token response has no ID token")
	}
	idt, err := prov.Verifier(&oidc.Config{ClientID: c.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return ssoIdentity{}, fmt.Errorf("ID token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(pend.nonce)) != 1 {
		return ssoIdentity{}, errors.New("ID token: the nonce doesn't match")
	}
	if idt.Subject == "" || len(idt.Subject) > maxSubject {
		return ssoIdentity{}, errors.New("ID token: the subject is missing or too long")
	}
	var cl struct {
		Email         string   `json:"email"`
		EmailVerified flexBool `json:"email_verified"`
		Name          string   `json:"name"`
	}
	if err := idt.Claims(&cl); err != nil {
		return ssoIdentity{}, errors.New("ID token: unreadable claims")
	}
	if cl.Email == "" {
		// Some providers keep the address out of the ID token: ask UserInfo, for the same subject.
		ui, err := prov.UserInfo(ctx, oauth2.StaticTokenSource(tok))
		switch {
		case err != nil:
			slog.InfoContext(ctx, "sso: no email in the ID token, and UserInfo failed", "provider", c.cfg.ID)
		case ui.Subject != idt.Subject:
			return ssoIdentity{}, errors.New("UserInfo: the subject doesn't match the ID token's")
		default:
			var uc struct {
				Name string `json:"name"`
			}
			_ = ui.Claims(&uc)
			cl.Email, cl.EmailVerified = ui.Email, flexBool(ui.EmailVerified)
			if cl.Name == "" {
				cl.Name = uc.Name
			}
		}
	}
	return ssoIdentity{subject: idt.Subject, email: cl.Email, name: cl.Name, verified: bool(cl.EmailVerified)}, nil
}

var (
	errSSOUnverified      = errors.New("the provider gave no verified email")
	errSSONoAccount       = errors.New("no account, and no invite, for this email")
	errSSOLinkedElsewhere = errors.New("the account is linked to another identity of this provider")
)

// ssoLogin finds or makes the user for an identity and logs them in, in one transaction:
//
//   - an identity already linked logs in its user;
//   - else a verified (or trusted) email that is a user's links the identity to them, unless
//     they already have another identity from this issuer, which the same address must not
//     take over;
//   - else an email with pending invites makes an SSO-only account (no password) that accepts
//     all of them;
//   - else the login is refused.
func (s *Service) ssoLogin(ctx context.Context, c *ssoClient, id ssoIdentity) (User, string, error) {
	var u User
	var token string
	err := s.write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT u.id, u.email, u.name FROM user_identities i JOIN users u ON u.id = i.user_id
			WHERE i.issuer = ? AND i.subject = ?`, c.cfg.Issuer, id.subject).Scan(&u.ID, &u.Email, &u.Name)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if u, err = s.ssoNewIdentity(ctx, tx, c, id); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		token, err = s.insertSession(ctx, tx, u.ID)
		return err
	})
	return u, token, err
}

// ssoNewIdentity links an identity not seen before to a user, making the user if an invite
// allows it.
func (s *Service) ssoNewIdentity(ctx context.Context, tx *sql.Tx, c *ssoClient, id ssoIdentity) (User, error) {
	email, ok := normalizeEmail(id.email)
	if !ok || (!id.verified && !c.cfg.TrustEmail) {
		return User{}, errSSOUnverified
	}
	u := User{Email: email}
	err := tx.QueryRowContext(ctx, `SELECT id, name FROM users WHERE email = ?`, email).Scan(&u.ID, &u.Name)
	switch {
	case err == nil:
		var linked bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM user_identities WHERE user_id = ? AND issuer = ?)`,
			u.ID, c.cfg.Issuer).Scan(&linked); err != nil {
			return User{}, err
		}
		if linked {
			return User{}, errSSOLinkedElsewhere
		}
	case errors.Is(err, sql.ErrNoRows):
		if u, err = s.ssoInvitedUser(ctx, tx, email, id.name); err != nil {
			return User{}, err
		}
	default:
		return User{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO user_identities (issuer, subject, user_id) VALUES (?, ?, ?)`,
		c.cfg.Issuer, id.subject, u.ID)
	return u, err
}

// ssoInvitedUser makes the account of an email with pending invites, without a password,
// and accepts every one of them.
func (s *Service) ssoInvitedUser(ctx context.Context, tx *sql.Tx, email, name string) (User, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, workspace_id, email, role FROM invites
		WHERE email = ? AND accepted_at IS NULL AND expires_at > ? ORDER BY id`, email, stamp(s.now()))
	if err != nil {
		return User{}, err
	}
	var invites []pendingInvite
	for rows.Next() {
		var inv pendingInvite
		if err := rows.Scan(&inv.id, &inv.workspaceID, &inv.email, &inv.role); err != nil {
			_ = rows.Close()
			return User{}, err
		}
		invites = append(invites, inv)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return User{}, err
	}
	if len(invites) == 0 {
		return User{}, errSSONoAccount
	}
	u, err := insertUser(ctx, tx, email, ssoName(name, email), "")
	if err != nil {
		return User{}, err
	}
	for _, inv := range invites {
		if err := s.useInvite(ctx, tx, inv); err != nil {
			return User{}, err
		}
		// Two invites to one workspace: the first one's role stands.
		if err := join(ctx, tx, inv, u.ID); err != nil && !errors.Is(err, errAlreadyMember) {
			return User{}, err
		}
	}
	return u, nil
}

// ssoName is the new account's name: the provider's, or the email's local part, at most
// maxNameLen characters.
func ssoName(name, email string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		name = strings.TrimSpace(string([]rune(name)[:maxNameLen]))
	}
	return name
}
