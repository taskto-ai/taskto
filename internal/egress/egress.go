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

type Policy struct {
	restricted bool
	public     bool
	hosts      []string
	prefixes   []netip.Prefix
}

var Any = Policy{}

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

func MustParse(text string) Policy {
	p, err := Parse(text)
	if err != nil {
		panic(err)
	}
	return p
}

func (p Policy) IsAny() bool { return !p.restricted }

func (p Policy) String() string {
	if !p.restricted {
		return "any"
	}
	return strings.Join(p.Entries(), ",")
}

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

var ErrRefused = errors.New("the server's egress policy doesn't allow this endpoint")

type RefusedError struct {
	Host   string
	Addr   netip.Addr
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

func (e *RefusedError) APIError(field string) *apierr.Error {
	msg := e.Error()
	if field != "" {
		msg = field + ": " + msg
	}
	return apierr.New(http.StatusBadRequest, apierr.CodeEgressRefused, msg, apierr.Params{"host": e.Host})
}

type Resolver func(ctx context.Context, network, host string) ([]netip.Addr, error)

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
		addrs = nil
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

type Dialer struct {
	Policy  Policy
	Resolve Resolver
	Timeout time.Duration
	dial    func(ctx context.Context, network, address string) (net.Conn, error)
}

func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if !d.Policy.restricted {
		return d.connect(ctx, network, address, false)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if d.Policy.AllowsHost(host) {
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

func (d *Dialer) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("egress: unexpected address %q", address)
	}
	return d.Policy.CheckAddr(ap.Addr().String(), ap.Addr())
}

func (p Policy) Client(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     p.Transport(nil),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (p Policy) Transport(resolve Resolver) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if p.restricted {
		t.Proxy = nil
	}
	d := &Dialer{Policy: p, Resolve: resolve}
	t.DialContext = d.DialContext
	return t
}

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
