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

type bucket struct {
	burst int
	every time.Duration
	now   func() time.Time

	mu     sync.Mutex
	tokens int
	last   time.Time
}

func newBucket(burst int, every time.Duration, now func() time.Time) *bucket {
	return &bucket{burst: burst, every: every, now: now, tokens: burst, last: now()}
}

func (b *bucket) take() (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if now.Before(b.last) {
		b.last = now
	}
	if n := int(now.Sub(b.last) / b.every); n > 0 {
		b.tokens = min(b.burst, b.tokens+n)
		b.last = b.last.Add(time.Duration(n) * b.every)
	}
	if b.tokens == b.burst {
		b.last = now
	}
	if b.tokens == 0 {
		return false, b.last.Add(b.every).Sub(now)
	}
	b.tokens--
	return true, 0
}

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
