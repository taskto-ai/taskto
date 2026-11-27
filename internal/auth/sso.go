package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/ratelimit"
)

const (
	ssoTTL        = 10 * time.Minute
	ssoMaxPending = 10_000
	ssoTimeout    = 15 * time.Second
	maxNextLen    = 2048
	maxSubject    = 255
)

type sso struct {
	providers  []*ssoClient
	byID       map[string]*ssoClient
	timeout    time.Duration
	startLimit *ratelimit.Limiter

	mu      sync.Mutex
	pending map[string]ssoPending
}

type ssoClient struct {
	cfg SSOProvider

	hc      *http.Client
	tokenHC *http.Client
	guard   *guardTransport
	failed  atomic.Int32

	mu       sync.Mutex
	provider *oidc.Provider
	oauth    *oauth2.Config
	flight   *discovery
}

type discovery struct {
	done     chan struct{}
	provider *oidc.Provider
	err      error
}

type ssoPending struct {
	provider, nonce, verifier, next string
	expires                         time.Time
}

func newSSO(providers []SSOProvider, now func() time.Time) *sso {
	x := &sso{
		byID:       map[string]*ssoClient{},
		timeout:    ssoTimeout,
		startLimit: ratelimit.New(ipAttempts, attemptWindow, now),
		pending:    map[string]ssoPending{},
	}
	for _, p := range providers {
		c := &ssoClient{cfg: p}
		c.guard = &guardTransport{base: http.DefaultTransport, loopbackHTTP: strings.HasPrefix(p.Issuer, "http://")}
		rt := &statusTransport{base: c.guard, last: &c.failed}
		c.hc = &http.Client{Timeout: ssoTimeout, Transport: rt, CheckRedirect: sameOriginRedirect}
		c.tokenHC = &http.Client{Timeout: ssoTimeout, Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		x.providers = append(x.providers, c)
		x.byID[p.ID] = c
	}
	return x
}

var (
	errInsecureEndpoint = errors.New("refusing a URL that isn't https")
	errCrossOrigin      = errors.New("refusing a redirect to another origin")
)

func sameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	if !sameOrigin(req.URL, via[0].URL) {
		return errCrossOrigin
	}
	return nil
}

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return a.Scheme == b.Scheme && sameASCIIHost(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

func sameASCIIHost(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x >= utf8.RuneSelf || y >= utf8.RuneSelf {
			return false
		}
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

type statusTransport struct {
	base http.RoundTripper
	last *atomic.Int32
}

func (t *statusTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err == nil && resp.StatusCode >= 300 && resp.StatusCode <= math.MaxInt32 {
		t.last.Store(int32(resp.StatusCode))
	}
	return resp, err
}

func (c *ssoClient) fail(category string) error {
	return &ssoErr{category: category, status: int(c.failed.Load())}
}

func secureURL(u *url.URL, allowLoopbackHTTP bool) bool {
	if u == nil || u.Host == "" || u.User != nil {
		return false
	}
	return u.Scheme == "https" || (allowLoopbackHTTP && u.Scheme == "http" && isLoopbackHost(u.Hostname()))
}

type guardTransport struct {
	base         http.RoundTripper
	loopbackHTTP bool
}

func (g *guardTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !secureURL(r.URL, g.loopbackHTTP) {
		return nil, errInsecureEndpoint
	}
	return g.base.RoundTrip(r)
}

type ssoErr struct {
	category string
	status   int
}

func (e *ssoErr) Error() string {
	if e.status != 0 {
		return e.category + " (HTTP " + strconv.Itoa(e.status) + ")"
	}
	return e.category
}

func failure(category string) error { return &ssoErr{category: category} }

var knownOAuthErrors = map[string]bool{
	"invalid_request": true, "invalid_client": true, "invalid_grant": true, "unauthorized_client": true,
	"unsupported_grant_type": true, "unsupported_response_type": true, "invalid_scope": true,
	"access_denied": true, "server_error": true, "temporarily_unavailable": true,
	"interaction_required": true, "login_required": true, "account_selection_required": true,
	"consent_required": true, "invalid_request_uri": true, "invalid_request_object": true,
	"request_not_supported": true, "request_uri_not_supported": true, "registration_not_supported": true,
}

func oauthCode(code string) string {
	if knownOAuthErrors[code] {
		return code
	}
	return "other"
}

func (x *sso) discover(ctx context.Context, c *ssoClient) (*oidc.Provider, error) {
	c.mu.Lock()
	if c.provider != nil {
		p := c.provider
		c.mu.Unlock()
		return p, nil
	}
	f := c.flight
	if f == nil {
		f = &discovery{done: make(chan struct{})}
		c.flight = f
		go x.runDiscovery(context.WithoutCancel(ctx), c, f)
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, x.timeout)
	defer cancel()
	select {
	case <-f.done:
		return f.provider, f.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, failure("discovery timed out")
		}
		return nil, failure("discovery canceled")
	}
}

func (x *sso) runDiscovery(base context.Context, c *ssoClient, f *discovery) {
	ctx, cancel := context.WithTimeout(oidc.ClientContext(base, c.hc), x.timeout)
	defer cancel()
	c.failed.Store(0)
	p, err := oidc.NewProvider(ctx, c.cfg.Issuer)
	if err != nil {
		p = nil
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = failure("discovery timed out")
		} else {
			err = c.fail("discovery failed")
		}
	} else if err = checkEndpoints(c.cfg.Issuer, p); err != nil {
		p = nil
	}
	c.mu.Lock()
	if err == nil {
		c.provider = p
	}
	c.flight = nil
	c.mu.Unlock()
	f.provider, f.err = p, err
	close(f.done)
}

func checkEndpoints(issuer string, p *oidc.Provider) error {
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := p.Claims(&doc); err != nil {
		return failure("discovery document unreadable")
	}
	dev := strings.HasPrefix(issuer, "http://")
	for _, e := range []struct {
		name, value string
		optional    bool
	}{
		{"authorization_endpoint", p.Endpoint().AuthURL, false},
		{"token_endpoint", p.Endpoint().TokenURL, false},
		{"jwks_uri", doc.JWKSURI, false},
		{"userinfo_endpoint", p.UserInfoEndpoint(), true},
	} {
		if e.value == "" && e.optional {
			continue
		}
		u, err := url.Parse(e.value)
		if err != nil || !secureURL(u, dev) {
			return failure("discovery: " + e.name + " is missing or isn't an https URL")
		}
	}
	return nil
}

func (s *Service) ssoRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/login/methods", refuseTokens(s.handleLoginMethods))
	mux.Handle("POST /api/sso/{provider}/start", refuseTokens(s.handleSSOStart))
	mux.Handle("GET /api/sso/{provider}/callback", refuseTokens(s.handleSSOCallback))
}

func (s *Service) handleLoginMethods(w http.ResponseWriter, _ *http.Request) {
	type method struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	list := []method{}
	for _, c := range s.sso.providers {
		list = append(list, method{c.cfg.ID, c.cfg.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"password": !s.opts.DisablePasswordLogin, "sso": list, "reset_by_email": s.ResetByEmail()})
}

func (s *Service) ssoCookieName() string {
	if s.opts.InsecureCookies {
		return "taskto_sso"
	}
	return "__Host-taskto_sso"
}

func (s *Service) ssoCookie(value string, maxAge int) *http.Cookie {
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
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.oauth == nil {
		c.oauth = &oauth2.Config{
			ClientID:     c.cfg.ClientID,
			ClientSecret: c.cfg.ClientSecret,
			Endpoint:     p.Endpoint(),
			RedirectURL:  s.opts.PublicURL + "/api/sso/" + c.cfg.ID + "/callback",
			Scopes:       c.cfg.Scopes,
		}
	}
	return c.oauth
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

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
	if ok, wait := s.sso.startLimit.Take(s.clientIP(r)); !ok {
		tooMany(w, wait)
		return
	}
	prov, err := s.sso.discover(r.Context(), c)
	if err != nil {
		slog.WarnContext(r.Context(), "sso: provider discovery failed", "provider", c.cfg.ID, "failure", err.Error())
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

func (x *sso) take(now time.Time, state, provider string) (ssoPending, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	p, ok := x.pending[state]
	if !ok || p.provider != provider {
		return ssoPending{}, false
	}
	delete(x.pending, state)
	return p, now.Before(p.expires)
}

func ssoFailure(w http.ResponseWriter, code apierr.Code) {
	redirectSeeOther(w, "/?sso_error="+url.QueryEscape(string(code)))
}

func redirectSeeOther(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusSeeOther)
}

func (s *Service) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
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
	pend, ok := s.sso.take(s.now(), state, c.cfg.ID)
	if !ok {
		ssoFailure(w, apierr.CodeSSOExpired)
		return
	}
	if e := q.Get("error"); e != "" {
		slog.InfoContext(r.Context(), "sso: the provider returned an error", "provider", c.cfg.ID, "error", oauthCode(e))
		s.ssoFailed(r, c, "provider_error")
		ssoFailure(w, apierr.CodeSSOFailed)
		return
	}
	id, err := s.ssoExchange(r.Context(), c, pend, q.Get("code"))
	if err != nil {
		slog.WarnContext(r.Context(), "sso: the sign-in failed", "provider", c.cfg.ID, "failure", err.Error())
		s.ssoFailed(r, c, "exchange_failed")
		ssoFailure(w, apierr.CodeSSOFailed)
		return
	}
	res, err := s.ssoLogin(r.Context(), c, id)
	if err != nil {
		code := apierr.CodeSSOFailed
		switch {
		case errors.Is(err, errSSOUnverified):
			code = apierr.CodeSSOEmailUnverified
		case errors.Is(err, errSSOGoogleEmail):
			code = apierr.CodeSSOGoogleEmailUntrusted
		case errors.Is(err, errSSONoAccount):
			code = apierr.CodeSSONoAccount
		case errors.Is(err, errSSOLinkedElsewhere):
			code = apierr.CodeSSOAccountLinkedElsewhere
		case errors.Is(err, errAccountDisabled):
			code = apierr.CodeAccountDisabled
		}
		if code == apierr.CodeSSOFailed {
			slog.ErrorContext(r.Context(), "sso: the login failed", "provider", c.cfg.ID, "err", err)
		} else {
			slog.InfoContext(r.Context(), "sso: the login was refused", "provider", c.cfg.ID, "reason", string(code))
		}
		s.ssoFailed(r, c, string(code))
		ssoFailure(w, code)
		return
	}
	if old := s.sessionToken(r); old != "" {
		if err := s.deleteSession(r.Context(), old); err != nil {
			slog.WarnContext(r.Context(), "auth: deleting the replaced session", "err", err)
		}
	}
	slog.InfoContext(r.Context(), "sso login", "provider", c.cfg.ID, "user_id", res.user.ID)
	s.setSessionCookie(w, res.token)
	next := pend.next
	if len(res.joined) > 0 && isInvitePath(next) {
		next = "/w/" + strconv.FormatInt(s.ssoLanding(r.Context(), next, res.joined, res.accepted), 10) + "/conversations"
	}
	redirectSeeOther(w, next)
}

func (s *Service) ssoFailed(r *http.Request, c *ssoClient, reason string) {
	audit.Log(r.Context(), s.db, audit.Event{Action: audit.SSOLoginFailed,
		Details: audit.Details{"provider": c.cfg.ID, "reason": reason}})
}

func (s *Service) ssoLanding(ctx context.Context, next string, joined, accepted []int64) int64 {
	if _, token, ok := strings.Cut(next, "#"); ok && token != "" {
		var id, ws int64
		err := s.db.QueryRowContext(ctx, `SELECT id, workspace_id FROM invites WHERE token_hash = ?`, hashToken(token)).Scan(&id, &ws)
		if err == nil && slices.Contains(accepted, id) {
			return ws
		}
	}
	return joined[0]
}

func isInvitePath(next string) bool {
	path, _, _ := strings.Cut(next, "#")
	path, _, _ = strings.Cut(path, "?")
	return path == "/invite" || strings.HasPrefix(path, "/invite/")
}

type ssoIdentity struct {
	subject, email, name string
	verified             bool
	hostedDomain         string
}

type flexBool bool

func (b *flexBool) UnmarshalJSON(d []byte) error {
	*b = string(d) == "true" || string(d) == `"true"`
	return nil
}

var googleIssuer = "https://accounts.google.com"

func googleEmailTrusted(email, hostedDomain string) bool {
	if strings.TrimSpace(hostedDomain) != "" {
		return true
	}
	_, domain, ok := strings.Cut(email, "@")
	return ok && isGmailDomain(domain)
}

func isGmailDomain(domain string) bool {
	const want = "gmail.com"
	if len(domain) != len(want) {
		return false
	}
	for i := 0; i < len(domain); i++ {
		c := domain[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != want[i] {
			return false
		}
	}
	return true
}

func (s *Service) ssoExchange(ctx context.Context, c *ssoClient, pend ssoPending, code string) (ssoIdentity, error) {
	if code == "" {
		return ssoIdentity{}, failure("no authorization code")
	}
	prov, err := s.sso.discover(ctx, c)
	if err != nil {
		return ssoIdentity{}, err
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, c.tokenHC), s.sso.timeout)
	defer cancel()
	tok, err := s.oauthConfig(c, prov).Exchange(ctx, code, oauth2.VerifierOption(pend.verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			e := &ssoErr{category: "token exchange refused: " + oauthCode(re.ErrorCode)}
			if re.Response != nil {
				e.status = re.Response.StatusCode
			}
			return ssoIdentity{}, e
		}
		return ssoIdentity{}, failure("token exchange failed")
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return ssoIdentity{}, failure("the token response has no ID token")
	}
	ctx = oidc.ClientContext(ctx, c.hc)
	c.failed.Store(0)
	idt, err := prov.Verifier(&oidc.Config{ClientID: c.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return ssoIdentity{}, c.fail("ID token not verified")
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(pend.nonce)) != 1 {
		return ssoIdentity{}, failure("ID token: the nonce doesn't match")
	}
	if idt.Subject == "" || len(idt.Subject) > maxSubject {
		return ssoIdentity{}, failure("ID token: the subject is missing or too long")
	}
	var cl struct {
		Email           string          `json:"email"`
		EmailVerified   flexBool        `json:"email_verified"`
		Name            string          `json:"name"`
		HostedDomain    string          `json:"hd"`
		AuthorizedParty json.RawMessage `json:"azp"`
	}
	if err := idt.Claims(&cl); err != nil {
		return ssoIdentity{}, failure("ID token: unreadable claims")
	}
	for _, aud := range idt.Audience {
		if aud != c.cfg.ClientID {
			return ssoIdentity{}, failure("ID token: the audience isn't this client's alone")
		}
	}
	if cl.AuthorizedParty != nil {
		var azp string
		if json.Unmarshal(cl.AuthorizedParty, &azp) != nil || azp != c.cfg.ClientID {
			return ssoIdentity{}, failure("ID token: the authorized party isn't this client")
		}
	}
	if cl.Email == "" {
		c.failed.Store(0)
		ui, err := prov.UserInfo(ctx, oauth2.StaticTokenSource(tok))
		switch {
		case err != nil:
			slog.InfoContext(ctx, "sso: no email in the ID token, and UserInfo failed", "provider", c.cfg.ID,
				"failure", c.fail("UserInfo failed").Error())
		case ui.Subject != idt.Subject:
			return ssoIdentity{}, failure("UserInfo: the subject doesn't match the ID token's")
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
	return ssoIdentity{subject: idt.Subject, email: cl.Email, name: cl.Name, verified: bool(cl.EmailVerified),
		hostedDomain: cl.HostedDomain}, nil
}

var (
	errSSOUnverified      = errors.New("the provider gave no verified email")
	errSSOGoogleEmail     = errors.New("a verified Google email outside Gmail and Google Workspace is not enough")
	errSSONoAccount       = errors.New("no account, and no invite, for this email")
	errSSOLinkedElsewhere = errors.New("the account is linked to another identity of this provider")
)

type ssoLoggedIn struct {
	user     User
	token    string
	joined   []int64
	accepted []int64
}

type ssoJoined struct {
	workspaces, invites []int64
}

func (s *Service) ssoLogin(ctx context.Context, c *ssoClient, id ssoIdentity) (ssoLoggedIn, error) {
	var res ssoLoggedIn
	err := s.write(ctx, func(tx *sql.Tx) error {
		var u User
		err := tx.QueryRowContext(ctx, `
			SELECT u.id, u.email, u.name FROM user_identities i JOIN users u ON u.id = i.user_id
			WHERE i.issuer = ? AND i.subject = ?`, c.cfg.Issuer, id.subject).Scan(&u.ID, &u.Email, &u.Name)
		var joined ssoJoined
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if u, joined, err = s.ssoNewIdentity(ctx, tx, c, id); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		token, err := s.insertSession(ctx, tx, u.ID)
		if err != nil {
			return err
		}
		res = ssoLoggedIn{user: u, token: token, joined: joined.workspaces, accepted: joined.invites}
		return audit.Record(ctx, tx, audit.Event{Actor: u.ID, Action: audit.SSOLoginSucceeded,
			TargetType: audit.TargetUser, Target: u.ID, Details: audit.Details{"provider": c.cfg.ID}})
	})
	return res, err
}

func (s *Service) ssoNewIdentity(ctx context.Context, tx *sql.Tx, c *ssoClient, id ssoIdentity) (User, ssoJoined, error) {
	email, ok := normalizeEmail(id.email)
	if !ok || (!id.verified && !c.cfg.TrustEmail) {
		return User{}, ssoJoined{}, errSSOUnverified
	}
	if c.cfg.Issuer == googleIssuer && (!id.verified || !googleEmailTrusted(id.email, id.hostedDomain)) {
		return User{}, ssoJoined{}, errSSOGoogleEmail
	}
	u := User{Email: email}
	var joined ssoJoined
	created := false
	err := tx.QueryRowContext(ctx, `SELECT id, name FROM users WHERE email = ?`, email).Scan(&u.ID, &u.Name)
	switch {
	case err == nil:
		var linked bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM user_identities WHERE user_id = ? AND issuer = ?)`,
			u.ID, c.cfg.Issuer).Scan(&linked); err != nil {
			return User{}, ssoJoined{}, err
		}
		if linked {
			return User{}, ssoJoined{}, errSSOLinkedElsewhere
		}
	case errors.Is(err, sql.ErrNoRows):
		if u, joined, err = s.ssoInvitedUser(ctx, tx, email, id.name); err != nil {
			return User{}, ssoJoined{}, err
		}
		created = true
	default:
		return User{}, ssoJoined{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO user_identities (issuer, subject, user_id) VALUES (?, ?, ?)`,
		c.cfg.Issuer, id.subject, u.ID); err != nil {
		return User{}, ssoJoined{}, err
	}
	err = audit.Record(ctx, tx, audit.Event{Actor: u.ID, Action: audit.SSOIdentityLinked,
		TargetType: audit.TargetUser, Target: u.ID, Details: audit.Details{"provider": c.cfg.ID, "created_account": created}})
	return u, joined, err
}

func (s *Service) ssoInvitedUser(ctx context.Context, tx *sql.Tx, email, name string) (User, ssoJoined, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, workspace_id, email, role FROM invites
		WHERE email = ? AND accepted_at IS NULL AND expires_at > ? ORDER BY id`, email, stamp(s.now()))
	if err != nil {
		return User{}, ssoJoined{}, err
	}
	var invites []pendingInvite
	for rows.Next() {
		var inv pendingInvite
		if err := rows.Scan(&inv.id, &inv.workspaceID, &inv.email, &inv.role); err != nil {
			_ = rows.Close()
			return User{}, ssoJoined{}, err
		}
		invites = append(invites, inv)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return User{}, ssoJoined{}, err
	}
	if len(invites) == 0 {
		return User{}, ssoJoined{}, errSSONoAccount
	}
	u, err := insertUser(ctx, tx, email, ssoName(name, email), "")
	if err != nil {
		return User{}, ssoJoined{}, err
	}
	var joined ssoJoined
	for _, inv := range invites {
		if err := s.useInvite(ctx, tx, inv); err != nil {
			return User{}, ssoJoined{}, err
		}
		if err := s.join(ctx, tx, inv, u.ID); err != nil && !errors.Is(err, errAlreadyMember) {
			return User{}, ssoJoined{}, err
		}
		joined.invites = append(joined.invites, inv.id)
		if !slices.Contains(joined.workspaces, inv.workspaceID) {
			joined.workspaces = append(joined.workspaces, inv.workspaceID)
		}
	}
	return u, joined, nil
}

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
