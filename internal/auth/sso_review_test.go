package auth

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

// leak is a synthetic secret planted where a provider's answer might carry one.
const leak = "s3cr3t-LEAK-value"

// lockedBuffer is a log destination safe for the handler's goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLogs sends slog's default logger to a buffer until the test ends.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

// hitCounter returns a server and a count of the requests a server gets.
func hitCounter(t *testing.T, tls bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { n.Add(1); w.WriteHeader(http.StatusOK) })
	var srv *httptest.Server
	if tls {
		srv = httptest.NewTLSServer(h)
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv, &n
}

// ---- 1. Google ----

// googleEnv is a provider that counts as Google's: the fake's issuer stands in for it.
func googleEnv(t *testing.T, tweak ...func(*SSOProvider, *Options)) *ssoEnv {
	t.Helper()
	old := googleIssuer
	t.Cleanup(func() { googleIssuer = old })
	return newSSOEnv(t, append([]func(*SSOProvider, *Options){func(p *SSOProvider, _ *Options) { googleIssuer = p.Issuer }}, tweak...)...)
}

func TestSSOGoogleNeedsGmailOrAWorkspaceToLink(t *testing.T) {
	t.Run("a third-party address without hd links nothing", func(t *testing.T) {
		s := googleEnv(t)
		s.setup() // owner@example.com
		s.wantFailure(s.signIn(s.browser(), "", verified("owner@example.com")), apierr.CodeSSOGoogleEmailUntrusted)
		if n := s.count(`SELECT count(*) FROM user_identities`); n != 0 {
			t.Fatalf("identities = %d", n)
		}
	})
	t.Run("a Workspace account (hd) links", func(t *testing.T) {
		s := googleEnv(t)
		s.setup()
		b := s.browser()
		claims := verified("owner@example.com")
		claims["hd"] = "example.com"
		s.wantLogin(b, s.signIn(b, "", claims), "/", "owner@example.com")
	})
	t.Run("a gmail.com address links", func(t *testing.T) {
		s := googleEnv(t)
		_, ws := s.setup()
		s.addMember("pat@gmail.com", ws)
		b := s.browser()
		s.wantLogin(b, s.signIn(b, "", verified("Pat@Gmail.com")), "/", "pat@gmail.com")
	})
	t.Run("a look-alike of gmail.com doesn't", func(t *testing.T) {
		s := googleEnv(t)
		_, ws := s.setup()
		s.addMember("pat@notgmail.com", ws)
		s.addMember("pat@gmail.com.example.org", ws)
		s.wantFailure(s.signIn(s.browser(), "", verified("pat@notgmail.com")), apierr.CodeSSOGoogleEmailUntrusted)
		s.wantFailure(s.signIn(s.browser(), "", verified("pat@gmail.com.example.org")), apierr.CodeSSOGoogleEmailUntrusted)
	})
	t.Run("trust_email doesn't bypass it", func(t *testing.T) {
		s := googleEnv(t, func(p *SSOProvider, _ *Options) { p.TrustEmail = true })
		s.setup()
		s.wantFailure(s.signIn(s.browser(), "", map[string]any{"email": "owner@example.com"}), apierr.CodeSSOGoogleEmailUntrusted)
		s.wantFailure(s.signIn(s.browser(), "", map[string]any{"email": "owner@example.com", "email_verified": false}),
			apierr.CodeSSOGoogleEmailUntrusted)
		if n := s.count(`SELECT count(*) FROM user_identities`); n != 0 {
			t.Fatalf("identities = %d", n)
		}
	})
	t.Run("an identity already linked keeps working", func(t *testing.T) {
		s := googleEnv(t)
		s.setup()
		b := s.browser()
		claims := verified("owner@example.com")
		claims["hd"] = "example.com"
		s.wantLogin(b, s.signIn(b, "", claims), "/", "owner@example.com")
		// Later the token has no hd, and the address is a stranger's: the subject is the key.
		b2 := s.browser()
		s.wantLogin(b2, s.signIn(b2, "", verified("whoever@example.org")), "/", "owner@example.com")
	})
	t.Run("another issuer isn't held to it", func(t *testing.T) {
		s := newSSOEnv(t)
		s.setup()
		b := s.browser()
		s.wantLogin(b, s.signIn(b, "", verified("owner@example.com")), "/", "owner@example.com")
	})
}

func TestSSOGoogleNeedsGmailOrAWorkspaceForAnInvite(t *testing.T) {
	t.Run("without hd no account is made", func(t *testing.T) {
		s := googleEnv(t)
		owner, ws := s.setup()
		s.invite(owner, ws, "new@example.com", RoleMember)
		s.wantFailure(s.signIn(s.browser(), "", verified("new@example.com")), apierr.CodeSSOGoogleEmailUntrusted)
		if n := s.count(`SELECT count(*) FROM users`); n != 1 {
			t.Fatalf("users = %d", n)
		}
		if n := s.count(`SELECT count(*) FROM invites WHERE accepted_at IS NULL`); n != 1 {
			t.Fatalf("pending invites = %d, the refusal used one up", n)
		}
	})
	t.Run("with hd", func(t *testing.T) {
		s := googleEnv(t)
		owner, ws := s.setup()
		s.invite(owner, ws, "new@example.com", RoleMember)
		b := s.browser()
		claims := verified("new@example.com")
		claims["hd"] = "example.com"
		s.wantLogin(b, s.signIn(b, "", claims), "/", "new@example.com")
	})
	t.Run("a gmail.com address", func(t *testing.T) {
		s := googleEnv(t)
		owner, ws := s.setup()
		s.invite(owner, ws, "new@gmail.com", RoleMember)
		b := s.browser()
		s.wantLogin(b, s.signIn(b, "", verified("new@gmail.com")), "/", "new@gmail.com")
	})
}

func TestGoogleEmailTrusted(t *testing.T) {
	for _, c := range []struct {
		email, hd string
		want      bool
	}{
		{"a@gmail.com", "", true},
		{"a@example.com", "example.com", true},
		{"a@example.com", "", false},
		{"a@gmail.com.evil.test", "", false},
		{"a@notgmail.com", "", false},
		{"gmail.com@example.com", "", false},
	} {
		if got := googleEmailTrusted(c.email, c.hd); got != c.want {
			t.Errorf("googleEmailTrusted(%q, %q) = %v", c.email, c.hd, got)
		}
	}
}

// ---- 2. https on every provider endpoint ----

func TestSSORefusesDiscoveredEndpointsThatAreNotHTTPS(t *testing.T) {
	for _, field := range []string{"authorization_endpoint", "token_endpoint", "jwks_uri", "userinfo_endpoint"} {
		t.Run(field, func(t *testing.T) {
			target, hits := hitCounter(t, false)
			idp := newFakeIdPWith(t, func(f *fakeIdP) {
				f.useTLS = true
				f.discovery = map[string]any{field: target.URL + "/x"}
			})
			s := newSSOEnvWith(t, idp)
			rec := s.browser().do(http.MethodPost, "/api/sso/fake/start", map[string]string{})
			expect(t, rec, http.StatusBadGateway)
			if got := decode[map[string]string](t, rec)["code"]; got != "sso_failed" {
				t.Fatalf("code %q", got)
			}
			if n := hits.Load(); n != 0 {
				t.Fatalf("the plain-http endpoint got %d requests", n)
			}
		})
	}
	t.Run("a missing token endpoint", func(t *testing.T) {
		idp := newFakeIdPWith(t, func(f *fakeIdP) { f.useTLS = true; f.discovery = map[string]any{"token_endpoint": ""} })
		s := newSSOEnvWith(t, idp)
		expect(t, s.browser().do(http.MethodPost, "/api/sso/fake/start", map[string]string{}), http.StatusBadGateway)
	})
	t.Run("https endpoints are fine", func(t *testing.T) {
		s := newSSOEnvWith(t, newFakeIdPWith(t, func(f *fakeIdP) { f.useTLS = true }))
		s.setup()
		b := s.browser()
		s.wantLogin(b, s.signIn(b, "", verified("owner@example.com")), "/", "owner@example.com")
	})
}

func TestSSOFollowsNoRedirectOnTheTokenExchange(t *testing.T) {
	other, hits := hitCounter(t, true) // another origin, https, sharing the test certificate
	idp := newFakeIdPWith(t, func(f *fakeIdP) {
		f.useTLS = true
		f.handlers["/token"] = func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
		}
	})
	s := newSSOEnvWith(t, idp)
	s.setup()
	s.wantFailure(s.signIn(s.browser(), "", verified("owner@example.com")), apierr.CodeSSOFailed)
	if n := hits.Load(); n != 0 {
		t.Fatalf("the code and the client secret were replayed to another origin (%d requests)", n)
	}
}

func TestSSORefusesRedirectsToPlainHTTP(t *testing.T) {
	target, hits := hitCounter(t, false)
	idp := newFakeIdPWith(t, func(f *fakeIdP) {
		f.useTLS = true
		f.handlers["/.well-known/openid-configuration"] = func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/.well-known/openid-configuration", http.StatusFound)
		}
	})
	s := newSSOEnvWith(t, idp)
	expect(t, s.browser().do(http.MethodPost, "/api/sso/fake/start", map[string]string{}), http.StatusBadGateway)
	if n := hits.Load(); n != 0 {
		t.Fatalf("the redirect to plain http was followed (%d requests)", n)
	}
}

func TestGuardTransport(t *testing.T) {
	g := &guardTransport{base: http.DefaultTransport}
	for _, raw := range []string{"http://example.com/x", "http://127.0.0.1:1/x", "ftp://example.com", "https://user:pw@example.com/"} {
		req := httptest.NewRequest(http.MethodGet, raw, nil)
		u, _ := url.Parse(raw)
		req.URL = u
		resp, err := g.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "https") {
			t.Errorf("%s: got %v", raw, err)
		}
	}
}

// ---- 3. no secrets in logs ----

func TestSSOLogsAndAnswersCarryNoProviderText(t *testing.T) {
	logs := captureLogs(t)
	secretBody := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream says " + leak))
	}
	noLeak := func(t *testing.T, where string, parts ...string) {
		t.Helper()
		for _, p := range append(parts, logs.String()) {
			if strings.Contains(p, leak) {
				t.Fatalf("%s: the provider's text reached %q", where, p)
			}
		}
	}

	t.Run("discovery", func(t *testing.T) {
		idp := newFakeIdPWith(t, func(f *fakeIdP) { f.handlers["/.well-known/openid-configuration"] = secretBody })
		s := newSSOEnvWith(t, idp)
		rec := s.browser().do(http.MethodPost, "/api/sso/fake/start", map[string]string{})
		expect(t, rec, http.StatusBadGateway)
		noLeak(t, "discovery", rec.Body.String())
		if !strings.Contains(logs.String(), "discovery failed") {
			t.Fatalf("the failure wasn't logged: %s", logs.String())
		}
	})
	t.Run("token endpoint: a body", func(t *testing.T) {
		idp := newFakeIdPWith(t, func(f *fakeIdP) { f.handlers["/token"] = secretBody })
		s := newSSOEnvWith(t, idp)
		s.setup()
		rec := s.signIn(s.browser(), "", verified("owner@example.com"))
		s.wantFailure(rec, apierr.CodeSSOFailed)
		noLeak(t, "token", rec.Header().Get("Location"), rec.Body.String())
		if !strings.Contains(logs.String(), "HTTP 500") {
			t.Fatalf("the status wasn't logged: %s", logs.String())
		}
	})
	t.Run("token endpoint: an error code and description", func(t *testing.T) {
		idp := newFakeIdPWith(t, func(f *fakeIdP) {
			f.handlers["/token"] = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"` + leak + `","error_description":"` + leak + `"}`))
			}
		})
		s := newSSOEnvWith(t, idp)
		s.setup()
		rec := s.signIn(s.browser(), "", verified("owner@example.com"))
		s.wantFailure(rec, apierr.CodeSSOFailed)
		noLeak(t, "token error", rec.Header().Get("Location"), rec.Body.String())
	})
	t.Run("keys", func(t *testing.T) {
		idp := newFakeIdPWith(t, func(f *fakeIdP) { f.handlers["/keys"] = secretBody })
		s := newSSOEnvWith(t, idp)
		s.setup()
		rec := s.signIn(s.browser(), "", verified("owner@example.com"))
		s.wantFailure(rec, apierr.CodeSSOFailed)
		noLeak(t, "keys", rec.Header().Get("Location"), rec.Body.String())
	})
	t.Run("the redirect's error and error_description", func(t *testing.T) {
		s := newSSOEnv(t)
		b := s.browser()
		q := s.start(b, "")
		rec := b.do(http.MethodGet, "/api/sso/fake/callback?state="+url.QueryEscape(q.Get("state"))+
			"&error="+leak+"&error_description="+leak, nil)
		s.wantFailure(rec, apierr.CodeSSOFailed)
		noLeak(t, "callback error", rec.Header().Get("Location"), rec.Body.String())
		if !strings.Contains(logs.String(), "error=other") {
			t.Fatalf("the refusal wasn't logged: %s", logs.String())
		}
	})
	t.Run("a standard error code is still logged", func(t *testing.T) {
		s := newSSOEnv(t)
		b := s.browser()
		q := s.start(b, "")
		b.do(http.MethodGet, "/api/sso/fake/callback?error=access_denied&state="+url.QueryEscape(q.Get("state")), nil)
		if !strings.Contains(logs.String(), "error=access_denied") {
			t.Fatalf("logs: %s", logs.String())
		}
	})
}

// ---- 5. the invitee's landing ----

func TestSSOInviteScreenAfterSignIn(t *testing.T) {
	t.Run("an account made from its invites goes to its first workspace", func(t *testing.T) {
		s := newSSOEnv(t)
		owner, ws := s.setup()
		s.invite(owner, ws, "new@example.com", RoleMember)
		b := s.browser()
		s.wantLogin(b, s.signIn(b, "/invite#tok", verified("new@example.com")), fmt.Sprintf("/w/%d", ws), "new@example.com")
	})
	t.Run("an account that already exists is only signed in, and keeps the pending invite", func(t *testing.T) {
		s := newSSOEnv(t)
		owner, ws := s.setup()
		memberID := s.addMember("pat@example.com", ws)
		ws2 := s.addWorkspace(int64(s.count(`SELECT id FROM users WHERE email = 'owner@example.com'`)), "Second")
		_ = owner
		if _, err := s.db.Exec(`INSERT INTO invites (workspace_id, token_hash, email, role, created_by, expires_at)
			VALUES (?, 'h2', 'pat@example.com', 'member', ?, ?)`, ws2, memberID, stamp(s.svc.now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
		b := s.browser()
		s.wantLogin(b, s.signIn(b, "/invite#tok", verified("pat@example.com")), "/invite#tok", "pat@example.com")
		if n := s.count(`SELECT count(*) FROM invites WHERE email = 'pat@example.com' AND accepted_at IS NULL`); n != 1 {
			t.Fatalf("pending invites = %d: signing in must not join", n)
		}
		if n := s.count(`SELECT count(*) FROM memberships WHERE workspace_id = ? AND user_id = ?`, ws2, memberID); n != 0 {
			t.Fatal("joined without accepting")
		}
	})
	t.Run("another next is kept for a new account", func(t *testing.T) {
		s := newSSOEnv(t)
		owner, ws := s.setup()
		s.invite(owner, ws, "new@example.com", RoleMember)
		b := s.browser()
		s.wantLogin(b, s.signIn(b, "/boards", verified("new@example.com")), "/boards", "new@example.com")
	})
	t.Run("isInvitePath", func(t *testing.T) {
		for next, want := range map[string]bool{"/invite": true, "/invite#t": true, "/invite?a=b#t": true, "/invitee": false,
			"/": false, "/w/1": false, "/x/invite": false} {
			if got := isInvitePath(next); got != want {
				t.Errorf("isInvitePath(%q) = %v", next, got)
			}
		}
	})
}

// ---- 6. discovery doesn't hold a lock across the network ----

func TestSSODiscoveryIsSharedAndEachCallerKeepsItsOwnDeadline(t *testing.T) {
	var stall atomic.Bool
	var asked atomic.Int32
	stall.Store(true)
	idp := newFakeIdP(t)
	// Wrap discovery: stalled until released.
	inner := idp.srv.Config.Handler
	idp.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration") {
			asked.Add(1)
			if stall.Load() {
				<-r.Context().Done()
				return
			}
		}
		inner.ServeHTTP(w, r)
	})
	s := newSSOEnvWith(t, idp)
	const timeout = 400 * time.Millisecond
	s.svc.sso.timeout = timeout

	const callers = 12
	var wg sync.WaitGroup
	began := time.Now()
	codes := make([]int, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = s.browser().do(http.MethodPost, "/api/sso/fake/start", map[string]string{}).Code
		}()
	}
	wg.Wait()
	if took := time.Since(began); took > 3*timeout {
		t.Fatalf("%d callers against a stalled provider took %v, want about one timeout (%v)", callers, took, timeout)
	}
	for i, code := range codes {
		if code != http.StatusBadGateway {
			t.Fatalf("caller %d: %d", i, code)
		}
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("the provider was asked %d times, want one shared discovery", n)
	}

	// A caller with a shorter deadline of its own gives up at that, not at the timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	began = time.Now()
	if _, err := s.svc.sso.discover(ctx, s.svc.sso.byID["fake"]); err == nil {
		t.Fatal("discovery of a stalled provider succeeded")
	}
	if took := time.Since(began); took > timeout/2 {
		t.Fatalf("a caller with a 30 ms deadline waited %v", took)
	}

	// The failure was forgotten: the provider answering again is found by the next login, and
	// a success is kept.
	stall.Store(false)
	time.Sleep(timeout + 100*time.Millisecond) // the stalled attempt has ended
	expect(t, s.browser().do(http.MethodPost, "/api/sso/fake/start", map[string]string{}), http.StatusOK)
	before := asked.Load()
	expect(t, s.browser().do(http.MethodPost, "/api/sso/fake/start", map[string]string{}), http.StatusOK)
	if asked.Load() != before {
		t.Fatal("a discovered provider was asked again")
	}
}

// ---- 7. audience ----

func TestSSOAudience(t *testing.T) {
	s := newSSOEnv(t)
	s.setup()
	with := func(kv map[string]any) map[string]any {
		m := verified("owner@example.com")
		for k, v := range kv {
			m[k] = v
		}
		return m
	}
	for name, claims := range map[string]map[string]any{
		"another audience as well":            with(map[string]any{"aud": []string{testClientID, "other"}}),
		"another audience, azp is the client": with(map[string]any{"aud": []string{testClientID, "other"}, "azp": testClientID}),
		"another audience first":              with(map[string]any{"aud": []string{"other", testClientID}, "azp": "other"}),
		"azp is someone else":                 with(map[string]any{"azp": "other"}),
	} {
		t.Run(name, func(t *testing.T) {
			b := s.browser()
			s.wantFailure(s.signIn(b, "", claims), apierr.CodeSSOFailed)
			if got := b.do(http.MethodGet, "/api/me", nil); got.Code != http.StatusUnauthorized {
				t.Fatalf("logged in: %d", got.Code)
			}
		})
	}
	for name, claims := range map[string]map[string]any{
		"one audience in a list": with(map[string]any{"aud": []string{testClientID}}),
		"azp is the client":      with(map[string]any{"azp": testClientID}),
	} {
		t.Run(name, func(t *testing.T) {
			b := s.browser()
			s.wantLogin(b, s.signIn(b, "", claims), "/", "owner@example.com")
		})
	}
}

// ---- 8. a stale callback ----

func TestSSOStaleCallbackLeavesANewerAttemptsCookie(t *testing.T) {
	s := newSSOEnv(t)
	s.setup()
	b := s.browser()
	qa := s.start(b, "/a")
	qb := s.start(b, "/b") // replaces the cookie
	if c := b.cookies[s.svc.ssoCookieName()]; c == nil || c.Value != qb.Get("state") {
		t.Fatalf("cookie %+v", c)
	}

	// A's callback arrives late and fails: the state isn't the cookie's.
	codeA := s.idp.issue(qa.Get("nonce"), qa.Get("code_challenge"), verified("owner@example.com"))
	s.wantFailure(b.do(http.MethodGet, "/api/sso/fake/callback?state="+url.QueryEscape(qa.Get("state"))+"&code="+codeA, nil),
		apierr.CodeSSOExpired)
	if c := b.cookies[s.svc.ssoCookieName()]; c == nil || c.Value != qb.Get("state") {
		t.Fatal("A's failed callback cleared B's cookie")
	}

	// B's callback goes through.
	codeB := s.idp.issue(qb.Get("nonce"), qb.Get("code_challenge"), verified("owner@example.com"))
	rec := b.do(http.MethodGet, "/api/sso/fake/callback?state="+url.QueryEscape(qb.Get("state"))+"&code="+codeB, nil)
	s.wantLogin(b, rec, "/b", "owner@example.com")
	if _, ok := b.cookies[s.svc.ssoCookieName()]; ok {
		t.Fatal("the cookie outlived the sign-in it was for")
	}
}
