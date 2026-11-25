package integrations

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/egress"
)

func TestRequestBuilderRefusesHostsTheAdapterDoesNotOwn(t *testing.T) {
	stripeClient := NewClient(Lookup("stripe"), nil, egress.MustParse("public"))
	for name, url := range map[string]string{
		"the adapter's host":      "https://api.stripe.com/v1/balance",
		"its upper-cased host":    "https://API.STRIPE.COM/v1/balance",
		"a trailing dot":          "https://api.stripe.com./v1/balance",
		"the explicit https port": "https://api.stripe.com:443/v1/balance",
	} {
		if _, err := stripeClient.NewRequest(context.Background(), http.MethodGet, url, nil); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, url := range map[string]string{ //nolint:gosec // G101: URLs with credentials in them, which must be refused
		"another host":              "https://example.com/v1/balance",
		"a sibling host":            "https://files.stripe.com/v1/balance",
		"a look-alike":              "https://api.stripe.com.example.com/",
		"a look-alike prefix":       "https://evilapi.stripe.com/",
		"credentials":               "https://api.stripe.com@example.com/",
		"credentials on the host":   "https://user:pass@api.stripe.com/",
		"plain http":                "http://api.stripe.com/v1/balance",
		"another port":              "https://api.stripe.com:8443/",
		"a loopback address":        "https://127.0.0.1/",
		"an IPv6 loopback":          "https://[::1]/",
		"the metadata address":      "https://169.254.169.254/latest/meta-data/",
		"a relative URL":            "/v1/balance",
		"another scheme":            "ftp://api.stripe.com/",
		"an opaque URL":             "https:api.stripe.com",
		"a host of another adapter": "https://connect.squareup.com/v2/payments",
		"an empty port":             "https://api.stripe.com:/v1/balance",
		"no host":                   "https:///v1/balance",
	} {
		_, err := stripeClient.NewRequest(context.Background(), http.MethodGet, url, nil)
		if !errors.Is(err, ErrHostNotOwned) {
			t.Errorf("%s: err = %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "example") {
			t.Errorf("%s: the error quotes the URL: %v", name, err)
		}
	}
}

// Unicode case mapping must never make a look-alike equal an adapter's host: the dialer encodes
// the look-alike as IDNA and connects to another domain.
func TestRequestBuilderRefusesNonASCIIHosts(t *testing.T) {
	stripeClient := NewClient(Lookup("stripe"), nil, egress.MustParse("public"))
	for name, url := range map[string]string{
		"a dotted capital I (maps to i)":      "https://api.str\u0130pe.com/v1/balance",
		"a long s (maps to s)":                "https://api.\u017ftripe.com/v1/balance",
		"a Kelvin sign":                       "https://api.stripe.com.\u212a/",
		"a fullwidth dot":                     "https://api\uff0estripe\uff0ecom/",
		"an ideographic full stop":            "https://api\u3002stripe\u3002com/",
		"a fullwidth letter":                  "https://api.stripe.\uff43om/",
		"an escaped capital I":                "https://api.str%C4%B0pe.com/",
		"punycode of a look-alike":            "https://xn--api-stripe-w6a.com/",
		"punycode in place of the whole host": "https://xn--stripe-api.com/",
		"a zero-width space":                  "https://api.stripe\u200b.com/",
	} {
		if _, err := stripeClient.NewRequest(context.Background(), http.MethodGet, url, nil); !errors.Is(err, ErrHostNotOwned) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// The same holds for the tests' own endpoint, where the old comparison folded case by Unicode.
	c := NewClient(Lookup("stripe"), nil, egress.MustParse("any"), WithTestEndpoint("kfake.test:8080"))
	for name, url := range map[string]string{
		"the endpoint":      "http://kfake.test:8080/",
		"its capital forms": "http://KFAKE.test:8080/",
	} {
		if _, err := c.NewRequest(context.Background(), http.MethodGet, url, nil); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.NewRequest(context.Background(), http.MethodGet, "http://\u212afake.test:8080/", nil); !errors.Is(err, ErrHostNotOwned) {
		t.Errorf("a Kelvin sign for k at the test endpoint: err = %v", err)
	}
	// An endpoint that isn't ASCII is never set.
	odd := NewClient(Lookup("stripe"), nil, egress.MustParse("any"), WithTestEndpoint("\u212afake.test:8080"))
	if odd.testHost != "" {
		t.Errorf("a non-ASCII test endpoint was kept: %q", odd.testHost)
	}
}

func TestASCIILower(t *testing.T) {
	if got, ok := asciiLower("API.Stripe.COM"); !ok || got != "api.stripe.com" {
		t.Errorf("asciiLower = %q, %v", got, ok)
	}
	for _, s := range []string{"\u0130", "\u212a", "\u017f", "a\u00e9"} {
		if got, ok := asciiLower(s); ok || got != "" {
			t.Errorf("asciiLower(%q) = %q, %v", s, got, ok)
		}
	}
}

// A request that wasn't made by NewRequest is checked again where it is sent, as completely.
func TestDoChecksWhatTheBuilderChecks(t *testing.T) {
	c := NewClient(Lookup("stripe"), nil, egress.MustParse("any"))
	ok, err := c.NewRequest(context.Background(), http.MethodGet, "https://api.stripe.com/v1/balance", nil)
	if err != nil {
		t.Fatal(err)
	}
	withHost := func(host string) *http.Request {
		r := ok.Clone(context.Background())
		r.Host = host
		return r
	}
	for name, req := range map[string]*http.Request{
		"a nil request":               nil,
		"no URL":                      {},
		"user info":                   {Method: http.MethodGet, URL: &url.URL{Scheme: "https", User: url.User("u"), Host: "api.stripe.com"}},
		"an opaque URL":               {Method: http.MethodGet, URL: &url.URL{Scheme: "https", Opaque: "//api.stripe.com/x"}},
		"an empty port":               {Method: http.MethodGet, URL: &url.URL{Scheme: "https", Host: "api.stripe.com:"}},
		"a Host header of another":    withHost("example.com"),
		"a Host header with a port":   withHost("api.stripe.com:8443"),
		"a non-ASCII Host header":     withHost("api.str\u0130pe.com"),
		"a Host header of a sibling":  withHost("files.stripe.com"),
		"a plain-http scheme":         {Method: http.MethodGet, URL: &url.URL{Scheme: "http", Host: "api.stripe.com"}},
		"a non-ASCII host in the URL": {Method: http.MethodGet, URL: &url.URL{Scheme: "https", Host: "api.str\u0130pe.com"}},
	} {
		resp, err := c.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrHostNotOwned) {
			t.Errorf("%s: Do: %v", name, err)
		}
	}
}

// A request that wasn't made by NewRequest is checked again where it is sent.
func TestDoRefusesARequestNotMadeByTheBuilder(t *testing.T) {
	c := NewClient(Lookup("stripe"), nil, egress.MustParse("any"))
	req, err := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrHostNotOwned) {
		t.Fatalf("Do: %v", err)
	}
}

// Square's hosts follow its environment, a choice: a sandbox integration never reaches production.
func TestSquareHostsFollowTheEnvironment(t *testing.T) {
	sq := Lookup("square")
	sandbox := NewClient(sq, map[string]string{"environment": "sandbox"}, egress.MustParse("public"))
	production := NewClient(sq, map[string]string{"environment": "production"}, egress.MustParse("public"))
	ctx := context.Background()
	for _, tt := range []struct {
		name   string
		client *Client
		url    string
		ok     bool
	}{
		{"sandbox to sandbox", sandbox, "https://connect.squareupsandbox.com/v2/locations", true},
		{"sandbox to production", sandbox, "https://connect.squareup.com/v2/locations", false},
		{"production to production", production, "https://connect.squareup.com/v2/locations", true},
		{"production to sandbox", production, "https://connect.squareupsandbox.com/v2/locations", false},
		{"production to Stripe", production, "https://api.stripe.com/v1/balance", false},
	} {
		_, err := tt.client.NewRequest(ctx, http.MethodGet, tt.url, nil)
		if (err == nil) != tt.ok {
			t.Errorf("%s: err = %v", tt.name, err)
		}
	}
}

func TestEveryProviderOwnsAFixedHost(t *testing.T) {
	for _, p := range Providers() {
		for _, settings := range []map[string]string{nil, {"environment": "sandbox"}, {"environment": "production"}} {
			hosts := p.Hosts(settings)
			if len(hosts) == 0 {
				t.Errorf("%s owns no host", p.ID())
			}
			for _, h := range hosts {
				if h != strings.ToLower(h) || strings.ContainsAny(h, "/:@ ") || !strings.Contains(h, ".") {
					t.Errorf("%s: host %q isn't a bare lower-case name", p.ID(), h)
				}
			}
		}
	}
	if Lookup("stripe").APIVersion() == "" {
		t.Error("Stripe's API version isn't pinned")
	}
}

// The tests reach a fake provider on loopback only through the constructor option, with the
// policy that allows it; the server's default policy refuses the address, and a redirect is an
// answer, never followed.
func TestClientReachesAFakeProviderAndStopsAtRedirects(t *testing.T) {
	var hits int
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer fake.Close()
	host := strings.TrimPrefix(fake.URL, "http://")

	c := NewClient(Lookup("stripe"), nil, egress.MustParse("any"), WithTestEndpoint(host))
	req, err := c.NewRequest(context.Background(), http.MethodGet, fake.URL+"/ok", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	req, _ = c.NewRequest(context.Background(), http.MethodGet, fake.URL+"/redirect", nil)
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || hits != 2 {
		t.Fatalf("a redirect was followed: status %d, %d hits", resp.StatusCode, hits)
	}

	// The default policy refuses the loopback address at the dialer, even for an allowed host.
	strict := NewClient(Lookup("stripe"), nil, egress.MustParse("public"), WithTestEndpoint(host))
	req, _ = strict.NewRequest(context.Background(), http.MethodGet, fake.URL+"/ok", nil)
	before := hits
	if resp, err := strict.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("the public policy reached a loopback address")
	} else if !errors.Is(err, egress.ErrRefused) {
		t.Fatalf("err = %v", err)
	}
	if hits != before {
		t.Fatal("the fake provider was reached")
	}

	// Without the option, nothing but the adapter's hosts is allowed, the fake's address included.
	plain := NewClient(Lookup("stripe"), nil, egress.MustParse("any"))
	if _, err := plain.NewRequest(context.Background(), http.MethodGet, fake.URL+"/ok", nil); !errors.Is(err, ErrHostNotOwned) {
		t.Fatalf("err = %v", err)
	}
}

// A service built with no policy uses "public".
func TestServiceClientDefaultsToThePublicPolicy(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer fake.Close()
	s := New(nil, Options{})
	c := s.Client(Lookup("stripe"), nil, WithTestEndpoint(strings.TrimPrefix(fake.URL, "http://")))
	req, _ := c.NewRequest(context.Background(), http.MethodGet, fake.URL, nil)
	if resp, err := c.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("the default policy reached a loopback address")
	}
}
