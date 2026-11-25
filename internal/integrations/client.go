package integrations

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"taskto.ai/taskto/internal/egress"
)

// ErrHostNotOwned is the refusal of a request to an address the provider's adapter doesn't own.
var ErrHostNotOwned = errors.New("integrations: the adapter doesn't own this host")

// callTimeout bounds one call to a provider, whatever the caller's context allows.
const callTimeout = 30 * time.Second

// Client makes an adapter's requests to its provider. It is the second lock on where an
// integration's credentials may be sent, after the egress policy: every request is built by
// NewRequest, which refuses any host the adapter doesn't own before the dialer is reached, and
// the HTTP client under it doesn't follow redirects and checks every connection's address
// (egress.Policy.Client). The hosts are constants of the adapter (Provider.Hosts): no setting,
// definition or request can add one, and only a constructor option (for tests) can.
type Client struct {
	http  *http.Client
	hosts []string // lower case, no port
	// testHost is a host:port the tests' fake provider listens on, reached over plain http. Empty
	// in the server: nothing in its configuration sets it.
	testHost string
}

// ClientOption changes how NewClient builds a Client. The options are for tests: the server
// builds its clients with none.
type ClientOption func(*Client)

// WithTestEndpoint lets the client send to hostport (a fake provider's listener) over plain
// http, besides the adapter's own hosts. Not reachable from configuration.
func WithTestEndpoint(hostport string) ClientOption {
	return func(c *Client) {
		if h, ok := asciiLower(hostport); ok {
			c.testHost = h
		}
	}
}

// NewClient returns the client for provider p of an integration with these settings, calling
// through the policy's HTTP client.
func NewClient(p Provider, settings map[string]string, policy egress.Policy, opts ...ClientOption) *Client {
	c := &Client{http: policy.Client(callTimeout)}
	for _, h := range p.Hosts(settings) {
		if h, ok := asciiLower(h); ok {
			c.hosts = append(c.hosts, h)
		}
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// BaseURL is where an adapter's requests to host (one of its own) go: https://host, or, in a test
// that gave the client a fake provider's address (WithTestEndpoint), that address over plain http.
// An adapter builds every address from it, so a test needs no change in the adapter.
func (c *Client) BaseURL(host string) string {
	if c.testHost != "" {
		return "http://" + c.testHost
	}
	return "https://" + host
}

// asciiLower lower-cases an ASCII host. A host with any other byte is refused (ok is false)
// before any case mapping: Unicode's lower-casing and folding turn "İ" into "i", the Kelvin sign
// into "k" and "ſ" into "s", so "api.strİpe.com" would equal "api.stripe.com" here while the
// dialer, which encodes it as IDNA, connects to another domain.
func asciiLower(s string) (string, bool) {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= utf8.RuneSelf:
			return "", false
		case 'A' <= c && c <= 'Z':
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b), true
}

// NewRequest builds a request to rawURL, which must be https, without credentials in it, on a
// port that is 443 or none, and at one of the adapter's hosts. Anything else is
// ErrHostNotOwned (the error never quotes the URL).
func (c *Client) NewRequest(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	u, err := url.Parse(rawURL)
	if err != nil || !c.owns(u) {
		return nil, ErrHostNotOwned
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, ErrHostNotOwned
	}
	return req, nil
}

// owns says whether u is an address the client may send to: a plain URL (no user info, not
// opaque) at one of the adapter's hosts over https on port 443 or none, or the tests' fake provider
// over http.
func (c *Client) owns(u *url.URL) bool {
	if u == nil || u.User != nil || u.Opaque != "" || u.Host == "" || strings.HasSuffix(u.Host, ":") {
		return false // "host:" has an empty port, which the parser takes for none
	}
	host, ok := asciiLower(u.Host)
	if !ok {
		return false
	}
	if c.testHost != "" && u.Scheme == "http" && host == c.testHost {
		return true
	}
	if u.Scheme != "https" {
		return false
	}
	if port := u.Port(); port != "" && port != "443" {
		return false
	}
	name, ok := asciiLower(strings.TrimSuffix(u.Hostname(), "."))
	if !ok || net.ParseIP(name) != nil {
		return false
	}
	return slices.Contains(c.hosts, name)
}

// Do sends a request built by NewRequest. A request built any other way is refused here too, and
// a redirect is an answer, not a hop.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req == nil || !c.owns(req.URL) || !hostMatches(req) {
		return nil, ErrHostNotOwned
	}
	return c.http.Do(req)
}

// hostMatches says whether the request's Host header, which the HTTP client sends in place of the
// URL's host, is the URL's own (or unset).
func hostMatches(req *http.Request) bool {
	if req.Host == "" {
		return true
	}
	host, ok := asciiLower(req.Host)
	want, _ := asciiLower(req.URL.Host)
	return ok && host == want
}
