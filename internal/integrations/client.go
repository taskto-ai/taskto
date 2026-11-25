package integrations

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

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
	return func(c *Client) { c.testHost = strings.ToLower(hostport) }
}

// NewClient returns the client for provider p of an integration with these settings, calling
// through the policy's HTTP client.
func NewClient(p Provider, settings map[string]string, policy egress.Policy, opts ...ClientOption) *Client {
	c := &Client{http: policy.Client(callTimeout)}
	for _, h := range p.Hosts(settings) {
		c.hosts = append(c.hosts, strings.ToLower(h))
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// NewRequest builds a request to rawURL, which must be https, without credentials in it, on a
// port that is 443 or none, and at one of the adapter's hosts. Anything else is
// ErrHostNotOwned (the error never quotes the URL).
func (c *Client) NewRequest(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.User != nil || u.Opaque != "" {
		return nil, ErrHostNotOwned
	}
	if !c.owns(u) {
		return nil, ErrHostNotOwned
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, ErrHostNotOwned
	}
	return req, nil
}

func (c *Client) owns(u *url.URL) bool {
	if c.testHost != "" && u.Scheme == "http" && strings.EqualFold(u.Host, c.testHost) {
		return true
	}
	if u.Scheme != "https" {
		return false
	}
	if port := u.Port(); port != "" && port != "443" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if net.ParseIP(host) != nil {
		return false
	}
	for _, h := range c.hosts {
		if host == h {
			return true
		}
	}
	return false
}

// Do sends a request built by NewRequest. A request built any other way is refused here too, and
// a redirect is an answer, not a hop.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if !c.owns(req.URL) {
		return nil, ErrHostNotOwned
	}
	return c.http.Do(req)
}
