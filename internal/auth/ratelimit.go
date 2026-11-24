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

// limiter counts attempts per key in fixed windows, in memory. A restart forgets the
// counts, which is fine for slowing down password guessing.
type limiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	hits map[string]*window
}

type window struct {
	count int
	ends  time.Time
}

// sweepAt is the number of keys above which take drops the expired windows.
const sweepAt = 10_000

func newLimiter(maxAttempts int, per time.Duration, now func() time.Time) *limiter {
	return &limiter{max: maxAttempts, window: per, now: now, hits: map[string]*window{}}
}

// take counts an attempt for key. It reports false, and how long until the window ends,
// if key has used up its attempts; that attempt isn't counted.
func (l *limiter) take(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.hits[key]
	if w == nil || !now.Before(w.ends) {
		if len(l.hits) >= sweepAt {
			l.sweep(now)
		}
		w = &window{ends: now.Add(l.window)}
		l.hits[key] = w
	}
	if w.count >= l.max {
		return false, w.ends.Sub(now)
	}
	w.count++
	return true, 0
}

// give hands back an attempt taken for key, for one that turned out not to count.
func (l *limiter) give(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w := l.hits[key]; w != nil && w.count > 0 {
		w.count--
	}
}

func (l *limiter) sweep(now time.Time) {
	for k, w := range l.hits {
		if !now.Before(w.ends) {
			delete(l.hits, k)
		}
	}
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
