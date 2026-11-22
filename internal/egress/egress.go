// Package egress decides which addresses the server may connect to when it calls a model
// provider's endpoint, and enforces it on every connection.
//
// Provider accounts name any base URL, and the server then calls it with the account's key:
// for an account test, for embeddings, and (through the agent runtime) for conversations. On
// a company network that lets a member make the server reach internal addresses, such as a
// cloud's metadata service or an admin interface. A Policy limits it:
//
//   - "any" (the default): no limit, for a single-user install or a trusted team;
//   - "public": public addresses only, no loopback, private, link-local, unique-local, CGNAT,
//     metadata or other special-purpose ranges, IPv4 and IPv6;
//   - a comma-separated allowlist of host names, IP addresses and CIDR prefixes, which may
//     include "public" as well: "public, ollama.internal, 10.1.2.0/24".
//
// Addresses are checked after DNS resolution, on every connection: the dialer resolves the
// name itself, checks each address and connects only to one it checked, so a name that
// resolves to a public address first and a private one later (DNS rebinding) gets nowhere.
// A refusal happens before connecting, so a key is never sent to a refused address.
//
// The agent runtime (agent/src/egress.ts) enforces the same rules, from the policy the
// server writes into its providers file.
package egress

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

// Policy says which addresses provider calls may connect to. The zero Policy allows any.
type Policy struct {
	restricted bool
	public     bool
	hosts      []string // lower case, without a trailing dot
	prefixes   []netip.Prefix
}

// Any is the policy that allows every address.
var Any = Policy{}

// Parse reads a policy: "any" (or ""), or a comma-separated list of "public", host names, IP
// addresses and CIDR prefixes.
func Parse(text string) (Policy, error) {
	text = strings.TrimSpace(text)
	if text == "" || strings.EqualFold(text, "any") {
		return Any, nil
	}
	p := Policy{restricted: true}
	for item := range strings.SplitSeq(text, ",") {
		item = strings.TrimSpace(item)
		switch {
		case item == "":
			continue
		case strings.EqualFold(item, "any"):
			return Policy{}, errors.New(`"any" can't be combined with other entries`)
		case strings.EqualFold(item, "public"):
			p.public = true
		case strings.Contains(item, "/"):
			prefix, err := netip.ParsePrefix(item)
			if err != nil {
				return Policy{}, fmt.Errorf("%q isn't a CIDR prefix", item)
			}
			// An IPv4-mapped prefix of at least /96 is that IPv4 prefix. A shorter one reaches
			// past ::ffff:0:0/96 into IPv6 space: it stays an IPv6 prefix, which matches no IPv4
			// address (they are compared unmapped), rather than widening to all of IPv4.
			prefix = prefix.Masked()
			if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
				prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
			}
			p.prefixes = append(p.prefixes, prefix)
		default:
			if addr, err := netip.ParseAddr(strings.Trim(item, "[]")); err == nil {
				if addr.Zone() != "" {
					return Policy{}, fmt.Errorf("%q: an address with a zone isn't supported", item)
				}
				addr = addr.Unmap()
				p.prefixes = append(p.prefixes, netip.PrefixFrom(addr, addr.BitLen()))
				continue
			}
			host := normalizeHost(item)
			if !validHost(host) {
				return Policy{}, fmt.Errorf("%q isn't a host name, IP address or CIDR prefix", item)
			}
			p.hosts = append(p.hosts, host)
		}
	}
	if !p.public && len(p.hosts) == 0 && len(p.prefixes) == 0 {
		return Policy{}, errors.New("want any, public, or a list of hosts and CIDR prefixes")
	}
	return p, nil
}

// MustParse is Parse for tests and constants; it panics on an error.
func MustParse(text string) Policy {
	p, err := Parse(text)
	if err != nil {
		panic(err)
	}
	return p
}

// IsAny reports whether p allows every address.
func (p Policy) IsAny() bool { return !p.restricted }

// String is p in the form Parse reads.
func (p Policy) String() string {
	if !p.restricted {
		return "any"
	}
	return strings.Join(p.Entries(), ",")
}

// Entries is p as a list of entries ("public", hosts, prefixes), or nil for any: what the
// agent runtime's providers file carries.
func (p Policy) Entries() []string {
	if !p.restricted {
		return nil
	}
	var out []string
	if p.public {
		out = append(out, "public")
	}
	out = append(out, p.hosts...)
	for _, prefix := range p.prefixes {
		if prefix.IsSingleIP() {
			out = append(out, prefix.Addr().String())
		} else {
			out = append(out, prefix.String())
		}
	}
	return out
}

// AllowsHost reports whether host is on p's list by name: every address it resolves to is
// then allowed, so list only names whose DNS you trust.
func (p Policy) AllowsHost(host string) bool {
	if !p.restricted {
		return true
	}
	host = normalizeHost(host)
	for _, h := range p.hosts {
		if h == host {
			return true
		}
	}
	return false
}

// CheckAddr returns nil if p allows connecting to addr, or a *RefusedError saying why not.
// host is the name it was resolved from, for the error.
func (p Policy) CheckAddr(host string, addr netip.Addr) error {
	if !p.restricted {
		return nil
	}
	addr = addr.Unmap().WithZone("")
	for _, prefix := range p.prefixes {
		if prefix.Contains(addr) {
			return nil
		}
	}
	reason := "not on the allowlist"
	if p.public {
		special := Special(addr)
		if special == "" {
			return nil
		}
		reason = "a " + special + " address"
	}
	return &RefusedError{Host: host, Addr: addr, Reason: reason}
}

// ErrRefused is wrapped by every refusal.
var ErrRefused = errors.New("the server's egress policy doesn't allow this endpoint")

// RefusedError is a connection p refused. Its message names the host and the address, which
// are the account's own, never a key.
type RefusedError struct {
	Host   string
	Addr   netip.Addr // invalid when the host is refused by name, before any resolution
	Reason string
}

func (e *RefusedError) Error() string {
	if !e.Addr.IsValid() {
		return fmt.Sprintf("%s: %s (%s)", ErrRefused, e.Host, e.Reason)
	}
	if e.Host == e.Addr.String() {
		return fmt.Sprintf("%s: %s is %s", ErrRefused, e.Host, e.Reason)
	}
	return fmt.Sprintf("%s: %s resolves to %s, %s", ErrRefused, e.Host, e.Addr, e.Reason)
}

func (e *RefusedError) Unwrap() error { return ErrRefused }

// APIError is the refusal as the API answers it: 400, egress_refused, with the host. field,
// if not empty, names the setting in the English message ("base_url: the server's…").
func (e *RefusedError) APIError(field string) *apierr.Error {
	msg := e.Error()
	if field != "" {
		msg = field + ": " + msg
	}
	return apierr.New(http.StatusBadRequest, apierr.CodeEgressRefused, msg, apierr.Params{"host": e.Host})
}

// Resolver looks up a host's addresses. net.DefaultResolver.LookupNetIP is one.
type Resolver func(ctx context.Context, network, host string) ([]netip.Addr, error)

// Check resolves host and returns a *RefusedError if p allows none of its addresses:
// what saving an account checks, so a forbidden endpoint is refused there and then. A host
// that can't be resolved now isn't refused here; every connection is checked anyway.
func (p Policy) Check(ctx context.Context, host string, resolve Resolver) error {
	if !p.restricted || p.AllowsHost(host) {
		return nil
	}
	host = strings.Trim(host, "[]")
	if addr, err := netip.ParseAddr(host); err == nil {
		return p.CheckAddr(host, addr)
	}
	if resolve == nil {
		resolve = net.DefaultResolver.LookupNetIP
	}
	addrs, err := resolve(ctx, "ip", host)
	if err != nil {
		return nil //nolint:nilerr // not refused: the connection's own check will be the judge
	}
	var refused error
	for _, addr := range addrs {
		err := p.CheckAddr(host, addr)
		if err == nil {
			return nil
		}
		refused = cmp.Or(refused, err)
	}
	return refused
}

// Dialer connects only to addresses its policy allows.
type Dialer struct {
	Policy Policy
	// Resolve defaults to net.DefaultResolver.LookupNetIP; tests give their own.
	Resolve Resolver
	// Timeout bounds each connection attempt; zero means 30 seconds.
	Timeout time.Duration
	// dial connects to a checked address; tests replace it.
	dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// DialContext resolves address's host, and connects to the first of its addresses the policy
// allows; it refuses with a *RefusedError if there is none.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if !d.Policy.restricted {
		return d.connect(ctx, network, address, false)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if d.Policy.AllowsHost(host) {
		// Listed by name: wherever it resolves to.
		return d.connect(ctx, network, address, false)
	}
	var addrs []netip.Addr
	if addr, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{addr}
	} else {
		resolve := d.Resolve
		if resolve == nil {
			resolve = net.DefaultResolver.LookupNetIP
		}
		lookup := "ip"
		switch network {
		case "tcp4":
			lookup = "ip4"
		case "tcp6":
			lookup = "ip6"
		}
		if addrs, err = resolve(ctx, lookup, host); err != nil {
			return nil, err
		}
	}
	var refused, failed error
	for _, addr := range addrs {
		if err := d.Policy.CheckAddr(host, addr); err != nil {
			refused = cmp.Or(refused, err)
			continue
		}
		conn, err := d.connect(ctx, network, net.JoinHostPort(addr.Unmap().String(), port), true)
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		failed = err
	}
	if failed != nil {
		return nil, failed
	}
	if refused != nil {
		return nil, refused
	}
	return nil, fmt.Errorf("egress: no addresses for %s", host)
}

// connect dials address; checked adds the socket-level check of control.
func (d *Dialer) connect(ctx context.Context, network, address string, checked bool) (net.Conn, error) {
	if d.dial != nil {
		return d.dial(ctx, network, address)
	}
	nd := &net.Dialer{Timeout: cmp.Or(d.Timeout, 30*time.Second), KeepAlive: 30 * time.Second}
	if checked {
		nd.Control = d.control
	}
	return nd.DialContext(ctx, network, address)
}

// control checks the address the socket is about to connect to, once more: the last word,
// whatever resolved it.
func (d *Dialer) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("egress: unexpected address %q", address)
	}
	return d.Policy.CheckAddr(ap.Addr().String(), ap.Addr())
}

// Client returns an HTTP client for calls to provider endpoints under p. It doesn't follow
// redirects, which would send the key on to wherever they point; a redirect is the answer.
// It uses no proxy from the environment when p is restricted, since a proxy would make the
// connection, not this client, and the check would be of the proxy's address.
func (p Policy) Client(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     p.Transport(nil),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Transport is the transport of Client; resolve is nil for the system's resolver.
func (p Policy) Transport(resolve Resolver) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if p.restricted {
		t.Proxy = nil
	}
	d := &Dialer{Policy: p, Resolve: resolve}
	t.DialContext = d.DialContext
	return t
}

// Refusal returns the refusal in err's chain, or nil.
func Refusal(err error) *RefusedError {
	var r *RefusedError
	if errors.As(err, &r) {
		return r
	}
	return nil
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func validHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
				return false
			}
		}
	}
	return true
}
