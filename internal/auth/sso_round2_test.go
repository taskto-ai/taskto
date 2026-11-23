package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"taskto.ai/taskto/internal/apierr"
)

// ---- redirects, per provider ----

// withDevProvider adds a second provider whose issuer is http on loopback, so that the server
// as a whole has the development exception somewhere. The provider under test must not get it.
func withDevProvider(t *testing.T) func(*SSOProvider, *Options) {
	dev := newFakeIdP(t) // plain http on 127.0.0.1
	return func(_ *SSOProvider, o *Options) {
		o.SSO = append(o.SSO, SSOProvider{ID: "dev", Name: "Dev", Issuer: dev.srv.URL, ClientID: testClientID,
			ClientSecret: testClientSecret, Scopes: []string{"openid", "email", "profile"}})
	}
}

func redirectTo(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target, http.StatusFound) }
}

func TestSSORedirectsNeverLeaveTheirOrigin(t *testing.T) {
	t.Run("an https provider is not given another provider's http exception", func(t *testing.T) {
		for _, what := range []string{"discovery", "keys", "userinfo"} {
			t.Run(what, func(t *testing.T) {
				target, hits := hitCounter(t, false) // http on loopback
				path := map[string]string{"discovery": "/.well-known/openid-configuration", "keys": "/keys", "userinfo": "/userinfo"}[what]
				idp := newFakeIdPWith(t, func(f *fakeIdP) {
					f.useTLS = true
					f.handlers[path] = redirectTo(target.URL + "/x")
				})
				s := newSSOEnvWith(t, idp, withDevProvider(t))
				s.setup()
				switch what {
				case "discovery":
					expect(t, s.browser().do(http.MethodPost, "/api/sso/fake/start", map[string]string{}), http.StatusBadGateway)
				case "keys":
					s.wantFailure(s.signIn(s.browser(), "", verified("owner@example.com")), apierr.CodeSSOFailed)
				default: // an ID token without an email makes the server ask UserInfo
					s.wantFailure(s.signIn(s.browser(), "", map[string]any{"email": nil}), apierr.CodeSSOEmailUnverified)
				}
				if n := hits.Load(); n != 0 {
					t.Fatalf("the redirect to http loopback was followed (%d requests)", n)
				}
				// The development provider itself still works.
				expect(t, s.browser().do(http.MethodPost, "/api/sso/dev/start", map[string]string{}), http.StatusOK)
			})
		}
	})

	t.Run("http to https and back to http", func(t *testing.T) {
		final, finalHits := hitCounter(t, false)
		var midHits atomic.Int32
		mid := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			midHits.Add(1)
			http.Redirect(w, r, final.URL+"/x", http.StatusFound)
		}))
		t.Cleanup(mid.Close)
		idp := newFakeIdPWith(t, func(f *fakeIdP) { // the provider is http on loopback itself
			f.handlers["/.well-known/openid-configuration"] = redirectTo(mid.URL + "/x")
		})
		s := newSSOEnvWith(t, idp)
		s.svc.sso.byID["fake"].guard.base = mid.Client().Transport // the middle hop's certificate is trusted
		expect(t, s.browser().do(http.MethodPost, "/api/sso/fake/start", map[string]string{}), http.StatusBadGateway)
		if finalHits.Load() != 0 || midHits.Load() != 0 {
			t.Fatalf("the chain was followed: %d and %d requests", midHits.Load(), finalHits.Load())
		}
	})

	t.Run("UserInfo's redirect to another origin carries no token", func(t *testing.T) {
		other, hits := hitCounter(t, true) // another https origin, with the test certificate
		idp := newFakeIdPWith(t, func(f *fakeIdP) {
			f.useTLS = true
			f.handlers["/userinfo"] = redirectTo(other.URL + "/x")
		})
		s := newSSOEnvWith(t, idp)
		s.setup()
		s.wantFailure(s.signIn(s.browser(), "", map[string]any{"email": nil}), apierr.CodeSSOEmailUnverified)
		if n := hits.Load(); n != 0 {
			t.Fatalf("UserInfo's redirect to another origin was followed (%d requests)", n)
		}
	})

	t.Run("a redirect within the origin is followed", func(t *testing.T) {
		idp := newFakeIdPWith(t, func(f *fakeIdP) { f.useTLS = true })
		inner := idp.srv.Config.Handler
		idp.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/keys" && r.URL.Query().Get("again") == "" {
				http.Redirect(w, r, "/keys?again=1", http.StatusFound)
				return
			}
			inner.ServeHTTP(w, r)
		})
		s := newSSOEnvWith(t, idp)
		s.setup()
		b := s.browser()
		s.wantLogin(b, s.signIn(b, "", verified("owner@example.com")), "/", "owner@example.com")
	})
}

func TestSameOrigin(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"https://a.example/x", "https://a.example/y?z", true},
		{"https://A.example", "https://a.example:443/", true},
		{"https://a.example", "http://a.example", false},
		{"https://a.example", "https://b.a.example", false},
		{"https://a.example", "https://a.example:8443", false},
		{"http://127.0.0.1:1", "http://127.0.0.1:2", false},
	} {
		a, _ := url.Parse(c.a)
		b, _ := url.Parse(c.b)
		if got := sameOrigin(a, b); got != c.want {
			t.Errorf("sameOrigin(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// ---- the invitee's landing ----

func TestSSONewAccountLandsOnTheRightWorkspace(t *testing.T) {
	// Two workspaces whose invites are in the opposite order to their ids: the second workspace
	// (the higher id) invited first.
	setup := func(t *testing.T) (s *ssoEnv, ws1, ws2 int64, tok1, tok2 string) {
		s = newSSOEnv(t)
		owner, ws1 := s.setup()
		ownerID := s.count(`SELECT id FROM users`)
		ws2 = s.addWorkspace(int64(ownerID), "Second")
		if ws2 <= ws1 {
			t.Fatalf("workspace ids %d, %d", ws1, ws2)
		}
		tok2 = s.invite(owner, ws2, "new@example.com", RoleMember)
		tok1 = s.invite(owner, ws1, "new@example.com", RoleMember)
		return s, ws1, ws2, tok1, tok2
	}
	t.Run("the invite that was opened", func(t *testing.T) {
		s, ws1, _, tok1, _ := setup(t)
		b := s.browser()
		s.wantLogin(b, s.signIn(b, "/invite#"+tok1, verified("new@example.com")),
			fmt.Sprintf("/w/%d/conversations", ws1), "new@example.com")
	})
	t.Run("otherwise the first accepted", func(t *testing.T) {
		s, _, ws2, _, _ := setup(t)
		b := s.browser()
		s.wantLogin(b, s.signIn(b, "/invite", verified("new@example.com")),
			fmt.Sprintf("/w/%d/conversations", ws2), "new@example.com")
	})
	t.Run("a token that isn't one of theirs is not followed", func(t *testing.T) {
		s, _, ws2, _, _ := setup(t)
		b := s.browser()
		s.wantLogin(b, s.signIn(b, "/invite#not-a-token", verified("new@example.com")),
			fmt.Sprintf("/w/%d/conversations", ws2), "new@example.com")
	})
}

// ---- a callback in flight, and a callback to the wrong provider ----

func TestSSOCallbackInFlightDoesNotBreakANewerAttempt(t *testing.T) {
	var blocked atomic.Value // the code whose token exchange waits
	blocked.Store("")
	entered, release := make(chan struct{}), make(chan struct{})
	idp := newFakeIdPWith(t, func(f *fakeIdP) {
		f.handlers["/token"] = func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			if r.PostForm.Get("code") == blocked.Load().(string) {
				close(entered)
				<-release
			}
			f.token(w, r)
		}
	})
	s := newSSOEnvWith(t, idp)
	s.setup()
	b := s.browser()

	qa := s.start(b, "/a")
	codeA := s.idp.issue(qa.Get("nonce"), qa.Get("code_challenge"), verified("owner@example.com"))
	blocked.Store(codeA)
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		done <- b.do(http.MethodGet, "/api/sso/fake/callback?state="+url.QueryEscape(qa.Get("state"))+"&code="+codeA, nil)
	}()
	<-entered // A's callback is at the token endpoint

	qb := s.start(b, "/b") // the person starts again in another tab: the cookie is B's now
	close(release)
	recA := <-done
	if recA.Code != http.StatusSeeOther || recA.Header().Get("Location") != "/a" {
		t.Fatalf("A: %d %q", recA.Code, recA.Header().Get("Location"))
	}

	// A's answer must not have taken B's cookie away.
	codeB := s.idp.issue(qb.Get("nonce"), qb.Get("code_challenge"), verified("owner@example.com"))
	rec := b.do(http.MethodGet, "/api/sso/fake/callback?state="+url.QueryEscape(qb.Get("state"))+"&code="+codeB, nil)
	s.wantLogin(b, rec, "/b", "owner@example.com")
}

// ---- Google: an hd that is empty ----

func TestSSOGoogleHostedDomainMustHaveContent(t *testing.T) {
	for name, hd := range map[string]any{"empty": "", "blank": "  \t", "absent": nil} {
		t.Run(name, func(t *testing.T) {
			s := googleEnv(t)
			s.setup()
			claims := verified("owner@example.com")
			claims["hd"] = hd
			s.wantFailure(s.signIn(s.browser(), "", claims), apierr.CodeSSOGoogleEmailUntrusted)
		})
	}
	t.Run("User@GMAIL.COM", func(t *testing.T) {
		s := googleEnv(t)
		_, ws := s.setup()
		s.addMember("user@gmail.com", ws)
		b := s.browser()
		s.wantLogin(b, s.signIn(b, "", verified("User@GMAIL.COM")), "/", "user@gmail.com")
	})
	t.Run("a look-alike that lower-cases to gmail.com", func(t *testing.T) {
		s := googleEnv(t)
		_, ws := s.setup()
		s.addMember("pat@gmail.com", ws)
		for _, email := range []string{"pat@gmaİl.com", "pat@gmaıl.com"} {
			s.wantFailure(s.signIn(s.browser(), "", verified(email)), apierr.CodeSSOGoogleEmailUntrusted)
		}
		if n := s.count(`SELECT count(*) FROM user_identities`); n != 0 {
			t.Fatalf("identities = %d", n)
		}
	})
}

// ---- the logs keep the status, never the body ----

func TestSSOLogsTheStatusOfEveryFailingCall(t *testing.T) {
	unavailable := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("upstream says " + leak))
	}
	for _, c := range []struct {
		name, path string
		claims     map[string]any
		start      bool // the failure is the start's
	}{
		{"discovery", "/.well-known/openid-configuration", nil, true},
		{"keys", "/keys", verified("owner@example.com"), false},
		{"userinfo", "/userinfo", map[string]any{"email": nil}, false},
		{"token endpoint", "/token", verified("owner@example.com"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := captureLogs(t)
			idp := newFakeIdPWith(t, func(f *fakeIdP) { f.handlers[c.path] = unavailable })
			s := newSSOEnvWith(t, idp)
			s.setup()
			var rec *httptest.ResponseRecorder
			if c.start {
				rec = s.browser().do(http.MethodPost, "/api/sso/fake/start", map[string]string{})
			} else {
				rec = s.signIn(s.browser(), "", c.claims)
			}
			out := logs.String()
			if !strings.Contains(out, "HTTP 503") {
				t.Fatalf("the status wasn't logged: %s", out)
			}
			if strings.Contains(out, leak) || strings.Contains(rec.Body.String(), leak) || strings.Contains(rec.Header().Get("Location"), leak) {
				t.Fatalf("the provider's body leaked: %s", out)
			}
		})
	}
}

func TestSSOLoopbackExceptionIsPerProvider(t *testing.T) {
	s := newSSOEnvWith(t, newFakeIdPWith(t, func(f *fakeIdP) { f.useTLS = true }), withDevProvider(t))
	if s.svc.sso.byID["fake"].guard.loopbackHTTP {
		t.Error("an https provider was given the http loopback exception")
	}
	if !s.svc.sso.byID["dev"].guard.loopbackHTTP {
		t.Error("an http loopback provider has no http loopback exception")
	}
}

func TestSSOTokenEndpointAuthStyleIsLearnedOnce(t *testing.T) {
	var basic, form atomic.Int32
	idp := newFakeIdPWith(t, func(f *fakeIdP) {
		// A token endpoint that takes the client's credentials in the form only.
		f.handlers["/token"] = func(w http.ResponseWriter, r *http.Request) {
			if _, _, ok := r.BasicAuth(); ok {
				basic.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
				return
			}
			form.Add(1)
			f.token(w, r)
		}
	})
	s := newSSOEnvWith(t, idp)
	s.setup()
	for range 3 {
		b := s.browser()
		s.wantLogin(b, s.signIn(b, "", verified("owner@example.com")), "/", "owner@example.com")
	}
	if basic.Load() != 1 || form.Load() != 3 {
		t.Fatalf("%d Basic attempts and %d form ones, want 1 and 3", basic.Load(), form.Load())
	}
}
