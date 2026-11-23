package auth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

const (
	testClientID     = "taskto-test-client"
	testClientSecret = "fake-client-secret" //nolint:gosec // G101: a fake, for the test provider
	testPublicURL    = "https://taskto.example.com"
)

// fakeIdP is an OpenID Connect provider: discovery, keys, a token endpoint that checks PKCE
// and signs ID tokens with RS256, and a UserInfo endpoint. There is no authorize page: tests
// play the browser and call issue with what the authorization URL asked for.
type fakeIdP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu           sync.Mutex
	codes        map[string]issuedCode
	userinfo     map[string]any
	badSignature bool // sign with a key the JWKS doesn't list

	// Set before the first request: serve over TLS, change what discovery says, or take over
	// an endpoint.
	useTLS    bool
	discovery map[string]any
	handlers  map[string]http.HandlerFunc
}

type issuedCode struct {
	challenge string
	claims    map[string]any
}

func newFakeIdP(t *testing.T) *fakeIdP { return newFakeIdPWith(t, nil) }

// newFakeIdPWith is newFakeIdP with setup run before the server starts.
func newFakeIdPWith(t *testing.T, setup func(*fakeIdP)) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{t: t, key: key, codes: map[string]issuedCode{}, userinfo: map[string]any{"sub": "sub-1"},
		handlers: map[string]http.HandlerFunc{}}
	if setup != nil {
		setup(f)
	}
	mux := http.NewServeMux()
	// route registers a handler, which a test may have replaced.
	route := func(path string, h http.HandlerFunc) {
		if o := f.handlers[path]; o != nil {
			h = o
		}
		mux.HandleFunc(path, h)
	}
	route("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint": f.srv.URL + "/token", "jwks_uri": f.srv.URL + "/keys",
			"userinfo_endpoint":                     f.srv.URL + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		for k, v := range f.discovery {
			doc[k] = v
		}
		writeTestJSON(w, doc)
	})
	route("/keys", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	route("/token", f.token)
	route("/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeTestJSON(w, f.userinfo)
	})
	if f.useTLS {
		f.srv = httptest.NewTLSServer(mux)
	} else {
		f.srv = httptest.NewServer(mux)
	}
	t.Cleanup(f.srv.Close)
	return f
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// issue is the provider's side of the authorization step: it makes a code for the nonce and
// PKCE challenge the request carried. claims override (or, as nil, remove) the ID token's.
func (f *fakeIdP) issue(nonce, challenge string, claims map[string]any) string {
	code, _ := randomToken()
	all := map[string]any{"nonce": nonce}
	for k, v := range claims {
		all[k] = v
	}
	f.mu.Lock()
	f.codes[code] = issuedCode{challenge: challenge, claims: all}
	f.mu.Unlock()
	return code
}

func (f *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	user, pass, basic := r.BasicAuth()
	if !basic {
		user, pass = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	f.mu.Lock()
	c, ok := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code"))
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if user != testClientID || pass != testClientSecret || !ok || b64(sum[:]) != c.challenge ||
		r.PostForm.Get("redirect_uri") != testPublicURL+"/api/sso/fake/callback" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	now := time.Now()
	claims := map[string]any{"iss": f.srv.URL, "aud": testClientID, "sub": "sub-1",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	for k, v := range c.claims {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	writeTestJSON(w, map[string]any{"access_token": "fake-access-token", "token_type": "Bearer",
		"expires_in": 3600, "id_token": f.sign(claims)})
}

func (f *fakeIdP) setUserinfo(v map[string]any) {
	f.mu.Lock()
	f.userinfo = v
	f.mu.Unlock()
}

func (f *fakeIdP) setBadSignature(v bool) {
	f.mu.Lock()
	f.badSignature = v
	f.mu.Unlock()
}

func (f *fakeIdP) sign(claims map[string]any) string {
	key := f.key
	f.mu.Lock()
	if f.badSignature {
		key, _ = rsa.GenerateKey(rand.Reader, 2048)
	}
	f.mu.Unlock()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	body, _ := json.Marshal(claims)
	signing := b64(header) + "." + b64(body)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return signing + "." + b64(sig)
}

// browser keeps cookies by name, as a real one does: the session's and the sign-in's.
type browser struct {
	e       *env
	mu      sync.Mutex // a test may have two requests of one browser in flight
	cookies map[string]*http.Cookie
}

func (e *env) browser() *browser { return &browser{e: e, cookies: map[string]*http.Cookie{}} }

func (b *browser) do(method, path string, body any) *httptest.ResponseRecorder {
	b.e.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		raw, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
	}
	// The provider's redirect is a cross-site navigation; the app's own calls are same-origin.
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if method == http.MethodGet && strings.Contains(path, "/callback") {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	}
	r.RemoteAddr = "192.0.2.1:1234"
	b.mu.Lock()
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	b.mu.Unlock()
	rec := httptest.NewRecorder()
	b.e.h.ServeHTTP(rec, r)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return rec
}

type ssoEnv struct {
	*env
	idp *fakeIdP
}

func newSSOEnv(t *testing.T, tweak ...func(*SSOProvider, *Options)) *ssoEnv {
	t.Helper()
	return newSSOEnvWith(t, newFakeIdP(t), tweak...)
}

func newSSOEnvWith(t *testing.T, idp *fakeIdP, tweak ...func(*SSOProvider, *Options)) *ssoEnv {
	t.Helper()
	p := SSOProvider{ID: "fake", Name: "Fake", Issuer: idp.srv.URL, ClientID: testClientID,
		ClientSecret: testClientSecret, Scopes: []string{"openid", "email", "profile"}}
	opts := Options{PublicURL: testPublicURL}
	for _, f := range tweak {
		f(&p, &opts)
	}
	opts.SSO = append([]SSOProvider{p}, opts.SSO...) // a tweak may add providers
	e := &ssoEnv{env: newEnv(t, opts), idp: idp}
	if idp.useTLS {
		// The test server's certificate is the only one trusted.
		e.svc.sso.byID[p.ID].guard.base = idp.srv.Client().Transport
	}
	return e
}

// start asks for the sign-in and returns the authorization URL's parameters.
func (s *ssoEnv) start(b *browser, next string) url.Values {
	s.t.Helper()
	var body any
	if next != "\x00" {
		body = map[string]string{"next": next}
	}
	rec := b.do(http.MethodPost, "/api/sso/fake/start", body)
	expect(s.t, rec, http.StatusOK)
	u, err := url.Parse(decode[map[string]string](s.t, rec)["url"])
	if err != nil || !strings.HasPrefix(u.String(), s.idp.srv.URL+"/authorize?") {
		s.t.Fatalf("authorization URL %v, %v", u, err)
	}
	q := u.Query()
	if q.Get("client_id") != testClientID || q.Get("response_type") != "code" ||
		q.Get("redirect_uri") != testPublicURL+"/api/sso/fake/callback" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("nonce") == "" || q.Get("state") == "" || !strings.Contains(q.Get("scope"), "openid") {
		s.t.Fatalf("authorization query %v", q)
	}
	return q
}

// signIn runs a whole sign-in as browser b, with the provider vouching for claims, and
// returns the callback's answer.
func (s *ssoEnv) signIn(b *browser, next string, claims map[string]any) *httptest.ResponseRecorder {
	s.t.Helper()
	q := s.start(b, next)
	code := s.idp.issue(q.Get("nonce"), q.Get("code_challenge"), claims)
	return b.do(http.MethodGet, "/api/sso/fake/callback?state="+url.QueryEscape(q.Get("state"))+"&code="+code, nil)
}

func (s *ssoEnv) wantFailure(rec *httptest.ResponseRecorder, code apierr.Code) {
	s.t.Helper()
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?sso_error="+string(code) {
		s.t.Fatalf("got %d %q, want a redirect to /?sso_error=%s", rec.Code, rec.Header().Get("Location"), code)
	}
}

func (s *ssoEnv) wantLogin(b *browser, rec *httptest.ResponseRecorder, next, email string) {
	s.t.Helper()
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != next {
		s.t.Fatalf("got %d %q, want a redirect to %s", rec.Code, rec.Header().Get("Location"), next)
	}
	got := b.do(http.MethodGet, "/api/me", nil)
	expect(s.t, got, http.StatusOK)
	if u := decode[me](s.t, got).User; u.Email != email {
		s.t.Fatalf("logged in as %q, want %q", u.Email, email)
	}
}

func verified(email string) map[string]any {
	return map[string]any{"email": email, "email_verified": true, "name": "Pat Example"}
}

func TestSSOLinksByVerifiedEmailThenLogsInBySubject(t *testing.T) {
	s := newSSOEnv(t)
	s.setup() // owner@example.com, with a password

	b := s.browser()
	rec := s.signIn(b, "/boards#x", verified("Owner@Example.com"))
	s.wantLogin(b, rec, "/boards#x", "owner@example.com")
	if n := s.count(`SELECT count(*) FROM user_identities WHERE issuer = ? AND subject = 'sub-1'`, s.idp.srv.URL); n != 1 {
		t.Fatalf("identities = %d", n)
	}
	if n := s.count(`SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("users = %d", n)
	}

	// Linked: the subject is the key, so the address may have changed or be unverified.
	b2 := s.browser()
	rec = s.signIn(b2, "", map[string]any{"email": "changed@example.com", "email_verified": false})
	s.wantLogin(b2, rec, "/", "owner@example.com")
}

func TestSSOCreatesAnInvitedAccountAndAcceptsEveryInvite(t *testing.T) {
	s := newSSOEnv(t)
	owner, ws1 := s.setup()
	ownerID := s.count(`SELECT id FROM users`)
	ws2 := s.addWorkspace(int64(ownerID), "Second")
	s.invite(owner, ws1, "new@example.com", RoleMember)
	// The second workspace's invite is made directly: owner's client is only an admin of the first.
	if _, err := s.db.Exec(`INSERT INTO invites (workspace_id, token_hash, email, role, created_by, expires_at)
		VALUES (?, 'h2', 'new@example.com', 'admin', ?, ?)`, ws2, ownerID, stamp(s.svc.now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}

	b := s.browser()
	rec := s.signIn(b, "/invite#tok", map[string]any{"email": "New@Example.com", "email_verified": true, "name": "  Nia  "})
	// Its invites are used up, so the invite screen would find nothing: it goes to the first
	// workspace joined.
	s.wantLogin(b, rec, fmt.Sprintf("/w/%d/conversations", ws1), "new@example.com")

	var name, hash string
	if err := s.db.QueryRow(`SELECT name, password_hash FROM users WHERE email = 'new@example.com'`).Scan(&name, &hash); err != nil {
		t.Fatal(err)
	}
	if name != "Nia" || hash != "" {
		t.Fatalf("name %q, hash %q", name, hash)
	}
	if n := s.count(`SELECT count(*) FROM memberships m JOIN users u ON u.id = m.user_id WHERE u.email = 'new@example.com'`); n != 2 {
		t.Fatalf("memberships = %d", n)
	}
	if n := s.count(`SELECT count(*) FROM invites WHERE email = 'new@example.com' AND accepted_at IS NULL`); n != 0 {
		t.Fatalf("%d invites still pending", n)
	}
}

func TestSSOWithoutAnInviteMakesNoAccount(t *testing.T) {
	s := newSSOEnv(t)
	owner, ws := s.setup()
	s.invite(owner, ws, "invited@example.com", RoleMember)
	s.invite(owner, ws, "late@example.com", RoleMember)

	// Someone else's invite doesn't count.
	s.wantFailure(s.signIn(s.browser(), "", verified("stranger@example.com")), apierr.CodeSSONoAccount)
	// Nor does an invite that has expired.
	s.advance(8 * 24 * time.Hour)
	s.wantFailure(s.signIn(s.browser(), "", verified("late@example.com")), apierr.CodeSSONoAccount)

	if n := s.count(`SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("users = %d, want only the owner", n)
	}
	if n := s.count(`SELECT count(*) FROM user_identities`); n != 0 {
		t.Fatalf("identities = %d", n)
	}
}

func TestSSOUnverifiedEmail(t *testing.T) {
	for name, claims := range map[string]map[string]any{
		"false":   {"email": "owner@example.com", "email_verified": false},
		"missing": {"email": "owner@example.com"},
		"no mail": {"email_verified": true},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSSOEnv(t)
			s.setup()
			s.wantFailure(s.signIn(s.browser(), "", claims), apierr.CodeSSOEmailUnverified)
			if n := s.count(`SELECT count(*) FROM user_identities`); n != 0 {
				t.Fatalf("identities = %d", n)
			}
		})
	}

	t.Run("string true is accepted", func(t *testing.T) {
		s := newSSOEnv(t)
		s.setup()
		b := s.browser()
		s.wantLogin(b, s.signIn(b, "", map[string]any{"email": "owner@example.com", "email_verified": "true"}), "/", "owner@example.com")
	})

	t.Run("trust_email", func(t *testing.T) {
		s := newSSOEnv(t, func(p *SSOProvider, _ *Options) { p.TrustEmail = true })
		s.setup()
		b := s.browser()
		s.wantLogin(b, s.signIn(b, "", map[string]any{"email": "owner@example.com"}), "/", "owner@example.com")
	})

	t.Run("an unverified email creates nothing even with an invite", func(t *testing.T) {
		s := newSSOEnv(t)
		owner, ws := s.setup()
		s.invite(owner, ws, "new@example.com", RoleMember)
		s.wantFailure(s.signIn(s.browser(), "", map[string]any{"email": "new@example.com", "email_verified": false}),
			apierr.CodeSSOEmailUnverified)
		if n := s.count(`SELECT count(*) FROM users`); n != 1 {
			t.Fatalf("users = %d", n)
		}
	})
}

func TestSSOUserInfoFillsAMissingEmail(t *testing.T) {
	s := newSSOEnv(t)
	s.setup()
	s.idp.setUserinfo(map[string]any{"sub": "sub-1", "email": "owner@example.com", "email_verified": true})
	b := s.browser()
	s.wantLogin(b, s.signIn(b, "", map[string]any{}), "/", "owner@example.com")

	// UserInfo for another subject is refused.
	s2 := newSSOEnv(t)
	s2.setup()
	s2.idp.setUserinfo(map[string]any{"sub": "someone-else", "email": "owner@example.com", "email_verified": true})
	s2.wantFailure(s2.signIn(s2.browser(), "", map[string]any{}), apierr.CodeSSOFailed)
}

func TestSSOStateIsBoundToTheBrowserAndWorksOnce(t *testing.T) {
	s := newSSOEnv(t)
	s.setup()
	claims := verified("owner@example.com")
	callback := func(b *browser, q url.Values, state string) *httptest.ResponseRecorder {
		code := s.idp.issue(q.Get("nonce"), q.Get("code_challenge"), claims)
		return b.do(http.MethodGet, "/api/sso/fake/callback?state="+url.QueryEscape(state)+"&code="+code, nil)
	}

	t.Run("a state that isn't the cookie's", func(t *testing.T) {
		b := s.browser()
		q := s.start(b, "")
		s.wantFailure(callback(b, q, "another-state"), apierr.CodeSSOExpired)
		// A callback that doesn't match leaves the cookie alone: it belongs to a sign-in that
		// may still be going on.
		if _, ok := b.cookies[s.svc.ssoCookieName()]; !ok {
			t.Fatal("the sign-in cookie was cleared by a callback that didn't match")
		}
	})
	t.Run("another browser's state", func(t *testing.T) {
		victim, attacker := s.browser(), s.browser()
		s.start(victim, "")
		qa := s.start(attacker, "")
		// The attacker sends the victim a callback with the attacker's own state.
		s.wantFailure(callback(victim, qa, qa.Get("state")), apierr.CodeSSOExpired)
	})
	t.Run("no cookie", func(t *testing.T) {
		b := s.browser()
		q := s.start(b, "")
		delete(b.cookies, s.svc.ssoCookieName())
		s.wantFailure(callback(b, q, q.Get("state")), apierr.CodeSSOExpired)
	})
	t.Run("no state", func(t *testing.T) {
		b := s.browser()
		s.start(b, "")
		s.wantFailure(b.do(http.MethodGet, "/api/sso/fake/callback?code=x", nil), apierr.CodeSSOExpired)
	})
	t.Run("expired", func(t *testing.T) {
		b := s.browser()
		q := s.start(b, "")
		s.advance(ssoTTL + time.Second)
		s.wantFailure(callback(b, q, q.Get("state")), apierr.CodeSSOExpired)
	})
	t.Run("replayed", func(t *testing.T) {
		b := s.browser()
		q := s.start(b, "")
		saved := *b.cookies[s.svc.ssoCookieName()]
		s.wantLogin(b, callback(b, q, q.Get("state")), "/", "owner@example.com")
		// The same cookie and state again, as a replayed link in the same browser.
		b.cookies[saved.Name] = &saved
		s.wantFailure(callback(b, q, q.Get("state")), apierr.CodeSSOExpired)
	})
	t.Run("the state of another provider", func(t *testing.T) {
		b := s.browser()
		q := s.start(b, "")
		s.svc.sso.byID["other"] = &ssoClient{cfg: SSOProvider{ID: "other"}}
		defer delete(s.svc.sso.byID, "other")
		rec := b.do(http.MethodGet, "/api/sso/other/callback?state="+url.QueryEscape(q.Get("state"))+"&code=x", nil)
		s.wantFailure(rec, apierr.CodeSSOExpired)
		// The wrong provider's callback consumed nothing: the right one still works.
		s.wantLogin(b, callback(b, q, q.Get("state")), "/", "owner@example.com")
	})
}

func TestSSOStartAndCallbackAnswers(t *testing.T) {
	s := newSSOEnv(t)
	b := s.browser()
	q := s.start(b, "/x")

	c := b.cookies[s.svc.ssoCookieName()]
	if c == nil || !strings.HasPrefix(c.Name, "__Host-") || c.Value != q.Get("state") || !c.HttpOnly || !c.Secure ||
		c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
		t.Fatalf("sso cookie %+v", c)
	}

	rec := b.do(http.MethodPost, "/api/sso/nope/start", map[string]string{})
	expect(t, rec, http.StatusNotFound)
	if got := decode[map[string]string](t, rec)["code"]; got != "sso_unknown_provider" {
		t.Fatalf("code %q", got)
	}
	s.wantFailure(b.do(http.MethodGet, "/api/sso/nope/callback?state=a&code=b", nil), apierr.CodeSSOUnknownProvider)

	// The start is a POST: the CSRF guard refuses it from another site, and a form's type.
	r := httptest.NewRequest(http.MethodPost, "/api/sso/fake/start", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	cross := httptest.NewRecorder()
	s.h.ServeHTTP(cross, r)
	expect(t, cross, http.StatusForbidden)
	r = httptest.NewRequest(http.MethodPost, "/api/sso/fake/start", strings.NewReader(`next=/`))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	form := httptest.NewRecorder()
	s.h.ServeHTTP(form, r)
	expect(t, form, http.StatusUnsupportedMediaType)

	// A body is optional, and a malformed one is refused.
	s.start(s.browser(), "\x00")
	expect(t, s.browser().do(http.MethodPost, "/api/sso/fake/start", map[string]any{"next": 5}), http.StatusBadRequest)
}

func TestSSOProviderErrorsAndBadTokens(t *testing.T) {
	s := newSSOEnv(t)
	s.setup()
	ok := verified("owner@example.com")
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for kk, vv := range ok {
			m[kk] = vv
		}
		m[k] = v
		return m
	}

	t.Run("a signature by another key", func(t *testing.T) {
		s.idp.setBadSignature(true)
		defer s.idp.setBadSignature(false)
		s.wantFailure(s.signIn(s.browser(), "", ok), apierr.CodeSSOFailed)
	})
	t.Run("access_denied", func(t *testing.T) {
		b := s.browser()
		q := s.start(b, "")
		s.wantFailure(b.do(http.MethodGet, "/api/sso/fake/callback?error=access_denied&state="+url.QueryEscape(q.Get("state")), nil),
			apierr.CodeSSOFailed)
	})
	t.Run("no code", func(t *testing.T) {
		b := s.browser()
		q := s.start(b, "")
		s.wantFailure(b.do(http.MethodGet, "/api/sso/fake/callback?state="+url.QueryEscape(q.Get("state")), nil), apierr.CodeSSOFailed)
	})
	t.Run("a code the provider doesn't know", func(t *testing.T) {
		b := s.browser()
		q := s.start(b, "")
		s.wantFailure(b.do(http.MethodGet, "/api/sso/fake/callback?code=made-up&state="+url.QueryEscape(q.Get("state")), nil),
			apierr.CodeSSOFailed)
	})
	for name, claims := range map[string]map[string]any{
		"nonce":      with("nonce", "another-nonce"),
		"no nonce":   with("nonce", nil),
		"audience":   with("aud", "someone-elses-client"),
		"issuer":     with("iss", "https://evil.example.com"),
		"expired":    with("exp", time.Now().Add(-time.Hour).Unix()),
		"no subject": with("sub", nil),
	} {
		t.Run(name, func(t *testing.T) {
			b := s.browser()
			s.wantFailure(s.signIn(b, "", claims), apierr.CodeSSOFailed)
			if got := b.do(http.MethodGet, "/api/me", nil); got.Code != http.StatusUnauthorized {
				t.Fatalf("logged in: %d", got.Code)
			}
		})
	}
}

func TestSSOWontLinkAnotherSubjectOfTheSameIssuer(t *testing.T) {
	s := newSSOEnv(t)
	s.setup()
	b := s.browser()
	s.wantLogin(b, s.signIn(b, "", verified("owner@example.com")), "/", "owner@example.com")

	// The same address under a new subject, say a recreated or second account at the provider.
	claims := verified("owner@example.com")
	claims["sub"] = "sub-2"
	s.wantFailure(s.signIn(s.browser(), "", claims), apierr.CodeSSOAccountLinkedElsewhere)
	if n := s.count(`SELECT count(*) FROM user_identities`); n != 1 {
		t.Fatalf("identities = %d", n)
	}
}

func TestSSOReplacesTheBrowsersSession(t *testing.T) {
	s := newSSOEnv(t)
	s.setup()
	b := s.browser()
	expect(t, b.do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": ownerPassword}), http.StatusOK)
	old := b.cookies[s.svc.cookieName()].Value
	s.wantLogin(b, s.signIn(b, "", verified("owner@example.com")), "/", "owner@example.com")
	if b.cookies[s.svc.cookieName()].Value == old {
		t.Fatal("the session wasn't replaced")
	}
	if n := s.count(`SELECT count(*) FROM sessions WHERE token_hash = ?`, hashToken(old)); n != 0 {
		t.Fatal("the old session lives")
	}
}

func TestSSODiscoveryIsLazy(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	s := newSSOEnv(t, func(p *SSOProvider, _ *Options) { p.Issuer = dead.URL }) // New didn't touch it
	rec := s.browser().do(http.MethodPost, "/api/sso/fake/start", map[string]string{})
	expect(t, rec, http.StatusBadGateway)
	if got := decode[map[string]string](t, rec)["code"]; got != "sso_failed" {
		t.Fatalf("code %q", got)
	}
}

func TestSafeNext(t *testing.T) {
	for next, want := range map[string]string{
		"":                                      "/",
		"/":                                     "/",
		"/boards":                               "/boards",
		"/invite#abc_-123":                      "/invite#abc_-123",
		"/a/b?c=d#e":                            "/a/b?c=d#e",
		"boards":                                "/",
		"//evil.example.com":                    "/",
		`/\evil.example.com`:                    "/",
		"/a\\b":                                 "/",
		"https://evil.example.com":              "/",
		"javascript:alert(1)":                   "/",
		"/a\nb":                                 "/",
		"/a\rb":                                 "/",
		"/a\x00b":                               "/",
		"/a\tb":                                 "/",
		"/a\u0085b":                             "/",
		"/\xff":                                 "/",
		"/" + strings.Repeat("a", maxNextLen-1): "/" + strings.Repeat("a", maxNextLen-1),
		"/" + strings.Repeat("a", maxNextLen):   "/",
	} {
		if got := safeNext(next); got != want {
			t.Errorf("safeNext(%.40q) = %.40q, want %.40q", next, got, want)
		}
	}
}

func TestSSOKeepsOnlyASafeNext(t *testing.T) {
	s := newSSOEnv(t)
	s.setup()
	for next, want := range map[string]string{"//evil.example.com/x": "/", "/ok/path#frag": "/ok/path#frag"} {
		b := s.browser()
		s.wantLogin(b, s.signIn(b, next, verified("owner@example.com")), want, "owner@example.com")
	}
}

func TestSSOPendingLoginsAreCapped(t *testing.T) {
	x := newSSO(nil, time.Now)
	now := time.Now()
	for i := range ssoMaxPending {
		if !x.put(now, fmt.Sprint(i), ssoPending{expires: now.Add(ssoTTL)}) {
			t.Fatalf("refused entry %d", i)
		}
	}
	if x.put(now, "one more", ssoPending{expires: now.Add(ssoTTL)}) {
		t.Fatal("accepted a login over the cap")
	}
	if !x.put(now.Add(ssoTTL), "later", ssoPending{expires: now.Add(2 * ssoTTL)}) {
		t.Fatal("expired entries weren't dropped to make room")
	}
	if len(x.pending) != 1 {
		t.Fatalf("pending = %d", len(x.pending))
	}
}

func TestSSOStartIsLimitedPerClient(t *testing.T) {
	s := newSSOEnv(t)
	b := s.browser()
	for range ipAttempts {
		expect(t, b.do(http.MethodPost, "/api/sso/fake/start", map[string]string{}), http.StatusOK)
	}
	rec := b.do(http.MethodPost, "/api/sso/fake/start", map[string]string{})
	expect(t, rec, http.StatusTooManyRequests)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
}

func TestLoginMethods(t *testing.T) {
	type methods struct {
		Password bool
		SSO      []struct{ ID, Name string }
	}
	plain := newEnv(t, Options{})
	got := decode[methods](t, plain.client().do(http.MethodGet, "/api/login/methods", nil))
	if !got.Password || got.SSO == nil || len(got.SSO) != 0 {
		t.Fatalf("plain: %+v", got)
	}
	if body := plain.client().do(http.MethodGet, "/api/login/methods", nil).Body.String(); !strings.Contains(body, `"sso":[]`) {
		t.Fatalf("sso must be an empty list, not null: %s", body)
	}

	s := newSSOEnv(t, func(_ *SSOProvider, o *Options) { o.DisablePasswordLogin = true })
	got = decode[methods](t, s.client().do(http.MethodGet, "/api/login/methods", nil))
	if got.Password || len(got.SSO) != 1 || got.SSO[0].ID != "fake" || got.SSO[0].Name != "Fake" {
		t.Fatalf("sso only: %+v", got)
	}
}

func TestPasswordLoginDisabled(t *testing.T) {
	s := newSSOEnv(t)
	owner, ws := s.setup()
	inviteToken := s.invite(owner, ws, "new@example.com", RoleMember)
	memberID := s.addMember("member@example.com", ws)
	reset := owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/password-reset", ws, memberID), nil)
	expect(t, reset, http.StatusCreated)
	resetToken := decode[map[string]string](t, reset)["token"]

	s.svc.opts.DisablePasswordLogin = true
	want := func(rec *httptest.ResponseRecorder) {
		t.Helper()
		expect(t, rec, http.StatusForbidden)
		if got := decode[map[string]string](t, rec)["code"]; got != "password_login_disabled" {
			t.Fatalf("code %q", got)
		}
	}
	want(s.client().do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": ownerPassword}))
	want(s.client().do(http.MethodPost, "/api/invites/accept", map[string]string{"token": inviteToken, "name": "New", "password": otherPassword}))
	want(s.client().do(http.MethodPost, "/api/password-reset", map[string]string{"token": resetToken, "password": otherPassword}))
	// Making a reset link is refused too: nobody could use it.
	want(owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/password-reset", ws, memberID), nil))
	if n := s.count(`SELECT count(*) FROM password_resets`); n != 1 {
		t.Fatalf("reset links = %d, want only the first", n)
	}

	// Nothing was used up by the refusals.
	if n := s.count(`SELECT count(*) FROM invites WHERE accepted_at IS NULL`); n != 1 {
		t.Fatalf("pending invites = %d", n)
	}
	s.svc.opts.DisablePasswordLogin = false
	expect(t, s.client().do(http.MethodPost, "/api/password-reset", map[string]string{"token": resetToken, "password": otherPassword}), http.StatusOK)
}

// addMember adds a user with a password to workspace as a member, and returns their ID.
func (e *env) addMember(email string, workspace int64) int64 {
	e.t.Helper()
	hash, err := e.svc.hasher.hash(context.Background(), otherPassword)
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.db.Exec(`INSERT INTO users (email, name, password_hash) VALUES (?, 'Member', ?)`, email, hash)
	if err != nil {
		e.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, workspace, id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestSSOOnlyUsersHaveNoPassword(t *testing.T) {
	s := newSSOEnv(t)
	owner, ws := s.setup()
	s.invite(owner, ws, "new@example.com", RoleMember)
	b := s.browser()
	s.wantLogin(b, s.signIn(b, "", verified("new@example.com")), "/", "new@example.com")
	userID := s.count(`SELECT id FROM users WHERE email = 'new@example.com'`)

	// Logging in with a password fails like a wrong one: never a 500, and nothing to tell it from an unknown address.
	rec := s.client().do(http.MethodPost, "/api/login", map[string]string{"email": "new@example.com", "password": otherPassword})
	expect(t, rec, http.StatusUnauthorized)
	if got := decode[map[string]string](t, rec)["code"]; got != "wrong_credentials" {
		t.Fatalf("code %q", got)
	}

	// Changing the password needs a current one they don't have.
	rec = b.do(http.MethodPost, "/api/me/password", map[string]string{"current_password": "anything-at-all", "new_password": otherPassword})
	expect(t, rec, http.StatusForbidden)
	if got := decode[map[string]string](t, rec)["code"]; got != "wrong_password" {
		t.Fatalf("code %q", got)
	}

	// An admin's reset link still gives them one.
	reset := owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/password-reset", ws, userID), nil)
	expect(t, reset, http.StatusCreated)
	expect(t, s.client().do(http.MethodPost, "/api/password-reset",
		map[string]string{"token": decode[map[string]string](t, reset)["token"], "password": otherPassword}), http.StatusOK)
	expect(t, s.client().do(http.MethodPost, "/api/login", map[string]string{"email": "new@example.com", "password": otherPassword}), http.StatusOK)
}
