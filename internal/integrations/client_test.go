package integrations

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
