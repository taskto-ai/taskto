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

var ErrHostNotOwned = errors.New("integrations: the adapter doesn't own this host")

const callTimeout = 30 * time.Second

type Client struct {
	http     *http.Client
	hosts    []string
	testHost string
}

type ClientOption func(*Client)

func WithTestEndpoint(hostport string) ClientOption {
	return func(c *Client) {
		if h, ok := asciiLower(hostport); ok {
			c.testHost = h
		}
	}
}

func WithoutConnectionReuse() ClientOption {
	return func(c *Client) {
		if t, ok := c.http.Transport.(*http.Transport); ok {
			t.DisableKeepAlives = true
		}
	}
}

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

func (c *Client) BaseURL(host string) string {
	if c.testHost != "" {
		return "http://" + c.testHost
	}
	return "https://" + host
}

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

func (c *Client) owns(u *url.URL) bool {
	if u == nil || u.User != nil || u.Opaque != "" || u.Host == "" || strings.HasSuffix(u.Host, ":") {
		return false
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

func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req == nil || !c.owns(req.URL) || !hostMatches(req) {
		return nil, ErrHostNotOwned
	}
	return c.http.Do(req)
}

func hostMatches(req *http.Request) bool {
	if req.Host == "" {
		return true
	}
	host, ok := asciiLower(req.Host)
	want, _ := asciiLower(req.URL.Host)
	return ok && host == want
}
