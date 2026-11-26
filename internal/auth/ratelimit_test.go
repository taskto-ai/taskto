package auth

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIP(t *testing.T) {
	for remote, want := range map[string]string{
		"192.0.2.1:1234":                 "192.0.2.1",
		"[::ffff:192.0.2.1]:1234":        "192.0.2.1",
		"[2001:db8:1:2:3:4:5:6]:443":     "2001:db8:1:2::/64",
		"[2001:db8:1:2:ffff::1]:443":     "2001:db8:1:2::/64",
		"[2001:db8:1:3::1]:443":          "2001:db8:1:3::/64",
		"not-an-address":                 "not-an-address",
		"[fe80::1%en0]:80":               "fe80::/64",
		"203.0.113.7:0":                  "203.0.113.7",
		"[::1]:8080":                     "::/64",
		"127.0.0.1:8080":                 "127.0.0.1",
		"[2001:db8::]:8080":              "2001:db8::/64",
		"[2001:db8:0:0:1:0:0:1]:8080":    "2001:db8::/64",
		"[2001:db8:aaaa:bbbb::1]:8080":   "2001:db8:aaaa:bbbb::/64",
		"[2001:db8:aaaa:bbbb:cc::1]:808": "2001:db8:aaaa:bbbb::/64",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		// Without trusted proxies, X-Forwarded-For is never read.
		r.Header.Set("X-Forwarded-For", "198.51.100.99")
		if got := (&Service{}).clientIP(r); got != want {
			t.Errorf("clientIP(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestClientIPBehindTrustedProxies(t *testing.T) {
	s := &Service{opts: Options{TrustedProxies: []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("2001:db8:ffff::/48"),
	}}}
	for _, tc := range []struct {
		name   string
		remote string
		xff    []string
		want   string
	}{
		{"untrusted peer, no header", "192.0.2.1:1", nil, "192.0.2.1"},
		{"untrusted peer spoofs the header", "192.0.2.1:1", []string{"198.51.100.7"}, "192.0.2.1"},
		{"untrusted peer spoofs a trusted address", "192.0.2.1:1", []string{"10.0.0.5"}, "192.0.2.1"},
		{"trusted proxy", "10.0.0.2:1", []string{"198.51.100.7"}, "198.51.100.7"},
		{"trusted proxy, no header", "10.0.0.2:1", nil, "10.0.0.2"},
		{"trusted proxy, empty header", "10.0.0.2:1", []string{""}, "10.0.0.2"},
		{"client prepends a fake address", "10.0.0.2:1", []string{"203.0.113.1, 198.51.100.7"}, "198.51.100.7"},
		{"client prepends a trusted address", "10.0.0.2:1", []string{"10.9.9.9, 198.51.100.7"}, "198.51.100.7"},
		{"two trusted proxies", "10.0.0.2:1", []string{"198.51.100.7, 10.0.0.3"}, "198.51.100.7"},
		{"spoof through two proxies", "10.0.0.2:1", []string{"203.0.113.1,198.51.100.7,10.0.0.3"}, "198.51.100.7"},
		{"header lines in order", "10.0.0.2:1", []string{"203.0.113.1", "198.51.100.7, 10.0.0.3"}, "198.51.100.7"},
		{"every hop trusted", "10.0.0.2:1", []string{"10.0.0.4, 10.0.0.3"}, "10.0.0.4"},
		{"garbage from the client is left of the client", "10.0.0.2:1", []string{"not-an-ip, 198.51.100.7"}, "198.51.100.7"},
		{"garbage next to the proxy", "10.0.0.2:1", []string{"198.51.100.7, not-an-ip"}, "10.0.0.2"},
		{"garbage behind a second proxy", "10.0.0.2:1", []string{"198.51.100.7, garbage, 10.0.0.3"}, "10.0.0.3"},
		{"entry with a port", "10.0.0.2:1", []string{"198.51.100.7:5555"}, "198.51.100.7"},
		{"IPv6 entry with a port", "10.0.0.2:1", []string{"[2001:db8:1:2::9]:5555"}, "2001:db8:1:2::/64"},
		{"IPv6 client", "10.0.0.2:1", []string{"2001:db8:1:2::9"}, "2001:db8:1:2::/64"},
		{"IPv4-mapped client", "10.0.0.2:1", []string{"::ffff:198.51.100.7"}, "198.51.100.7"},
		{"IPv6 trusted proxy", "[2001:db8:ffff::1]:1", []string{"198.51.100.7"}, "198.51.100.7"},
		{"IPv4-mapped trusted peer", "[::ffff:10.0.0.2]:1", []string{"198.51.100.7"}, "198.51.100.7"},
		{"neighbour in another /48", "[2001:db8:fffe::1]:1", []string{"198.51.100.7"}, "2001:db8:fffe::/64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := s.clientIP(r); got != tc.want {
				t.Errorf("clientIP(%s, %q) = %q, want %q", tc.remote, tc.xff, got, tc.want)
			}
		})
	}
}
