package auth

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"taskto.ai/taskto/internal/audit"
)

// bucket is a token bucket: burst starts at once, then one more every `every`, in memory. What it
// counts is requests taken in, never work still running, so whether it refuses depends only on
// the requests that came before and the clock.
type bucket struct {
	burst int
	every time.Duration
	now   func() time.Time

	mu     sync.Mutex
	tokens int
	last   time.Time // when the last token was added, while not full
}

func newBucket(burst int, every time.Duration, now func() time.Time) *bucket {
	return &bucket{burst: burst, every: every, now: now, tokens: burst, last: now()}
}

// take uses a token. It reports false, and how long until the next one, if there is none.
func (b *bucket) take() (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if now.Before(b.last) { // the clock went back
		b.last = now
	}
	if n := int(now.Sub(b.last) / b.every); n > 0 {
		b.tokens = min(b.burst, b.tokens+n)
		b.last = b.last.Add(time.Duration(n) * b.every)
	}
	if b.tokens == b.burst {
		b.last = now // full: the next token is a whole interval after this one is used
	}
	if b.tokens == 0 {
		return false, b.last.Add(b.every).Sub(now)
	}
	b.tokens--
	return true, 0
}

// clientIP is the key for a request's client address. It is the direct peer, unless the
// peer is a trusted proxy (Options.TrustedProxies): then it is the right-most address in
// X-Forwarded-For that isn't a trusted proxy. Each proxy appends the address it received
// the request from, so the entries right of that one were written by trusted proxies, and
// the ones left of it can be anything the client sent. An untrusted peer's header is
// ignored, so a client can't pick its own key. An IPv6 client usually controls a whole
// /64, so that is the unit.
func (s *Service) clientIP(r *http.Request) string {
	addr, host := s.client(r)
	if !addr.IsValid() {
		return host
	}
	if addr.Is6() {
		prefix, _ := addr.Prefix(64)
		return prefix.String()
	}
	return addr.String()
}

// client is the request's client address as clientIP finds it, in full; it is invalid for a peer
// that isn't an IP address, and host is then what the peer is.
func (s *Service) client(r *http.Request) (addr netip.Addr, host string) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err = netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, host
	}
	addr = addr.WithZone("").Unmap()
	if s.trustedProxy(addr) {
		addr = s.forwardedClient(r.Header.Values("X-Forwarded-For"), addr)
	}
	return addr, host
}

// ClientAddr puts the request's client address on its context, found as the login limits find it,
// for the audit log's events (internal/audit).
func (s *Service) ClientAddr(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr, host := s.client(r)
		if addr.IsValid() {
			host = addr.String()
		}
		next.ServeHTTP(w, r.WithContext(audit.WithClient(r.Context(), host)))
	})
}

func (s *Service) trustedProxy(addr netip.Addr) bool {
	for _, p := range s.opts.TrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// forwardedClient walks X-Forwarded-For from the right, from the trusted peer, and returns
// the first address that isn't a trusted proxy. If an entry isn't an address, the trusted
// proxy that wrote it is the last address known, and so the client; the same if every
// address is a trusted proxy.
func (s *Service) forwardedClient(headers []string, peer netip.Addr) netip.Addr {
	client := peer
	for i := len(headers) - 1; i >= 0; i-- {
		entries := strings.Split(headers[i], ",")
		for j := len(entries) - 1; j >= 0; j-- {
			addr, ok := forwardedAddr(entries[j])
			if !ok {
				return client
			}
			client = addr
			if !s.trustedProxy(addr) {
				return client
			}
		}
	}
	return client
}

// forwardedAddr parses one X-Forwarded-For entry: an IP address, or one with a port as
// some proxies write it.
func forwardedAddr(entry string) (netip.Addr, bool) {
	entry = strings.TrimSpace(entry)
	addr, err := netip.ParseAddr(entry)
	if err != nil {
		ap, err := netip.ParseAddrPort(entry)
		if err != nil {
			return netip.Addr{}, false
		}
		addr = ap.Addr()
	}
	return addr.WithZone("").Unmap(), true
}
