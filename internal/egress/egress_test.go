package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for text, want := range map[string]string{
		"":                          "any",
		"any":                       "any",
		" ANY ":                     "any",
		"public":                    "public",
		"public, Ollama.Internal.":  "public,ollama.internal",
		"10.1.2.0/24, 192.0.2.7":    "10.1.2.0/24,192.0.2.7",
		"10.1.2.3/24":               "10.1.2.0/24",
		"::ffff:10.0.0.0/104":       "10.0.0.0/8",
		"[fd00::1], fd00::/8, host": "host,fd00::1,fd00::/8",
	} {
		p, err := Parse(text)
		if err != nil || p.String() != want {
			t.Errorf("Parse(%q) = %q, %v; want %q", text, p.String(), err, want)
		}
	}
	for _, text := range []string{"any, public", "public, any", ",", "10.0.0.0/33", "fe80::1%eth0", "a b", "-x.example/", "host:80", "a..b"} {
		if p, err := Parse(text); err == nil {
			t.Errorf("Parse(%q) = %q; want an error", text, p)
		}
	}
	if !Any.IsAny() || MustParse("public").IsAny() || Any.Entries() != nil {
		t.Error("IsAny or Entries is wrong")
	}
}

func TestSpecial(t *testing.T) {
	for addr, want := range map[string]string{
		"8.8.8.8":              "",
		"1.1.1.1":              "",
		"2606:4700:4700::1111": "",
		"::ffff:8.8.8.8":       "",
		"64:ff9b::808:808":     "",
		"2002:808:808::1":      "",
		"127.0.0.1":            "loopback",
		"127.255.0.9":          "loopback",
		"::1":                  "loopback",
		"::ffff:127.0.0.1":     "loopback",
		"10.1.2.3":             "private",
		"172.16.0.1":           "private",
		"172.31.255.255":       "private",
		"192.168.1.1":          "private",
		"169.254.169.254":      "link-local",
		"fe80::1":              "link-local",
		"fd00:ec2::254":        "unique-local",
		"fc00::1":              "unique-local",
		"100.64.0.1":           "CGNAT",
		"100.100.100.200":      "CGNAT",
		"192.0.0.192":          "reserved",
		"0.0.0.0":              "reserved",
		"::":                   "reserved",
		"255.255.255.255":      "reserved",
		"224.0.0.1":            "multicast",
		"ff02::1":              "multicast",
		"64:ff9b::a00:1":       "private",
		"64:ff9b::a9fe:a9fe":   "link-local",
		"2002:7f00:1::1":       "loopback",
		"2001:db8::1":          "documentation",
		"2001::1":              "reserved",
		"fec0::1":              "reserved",
		"::127.0.0.1":          "reserved",
		"172.32.0.1":           "",
		"100.128.0.1":          "",
	} {
		if got := Special(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Special(%s) = %q; want %q", addr, got, want)
		}
	}
}

func TestCheckAddr(t *testing.T) {
	cases := []struct {
		policy, addr string
		ok           bool
	}{
		{"any", "169.254.169.254", true},
		{"public", "8.8.8.8", true},
		{"public", "10.0.0.1", false},
		{"public", "::ffff:169.254.169.254", false},
		{"10.0.0.0/8", "10.9.9.9", true},
		{"10.0.0.0/8", "8.8.8.8", false},
		{"10.0.0.0/8", "::ffff:10.0.0.1", true},
		{"public, 10.0.0.0/8", "10.0.0.1", true},
		{"public, 10.0.0.0/8", "192.168.0.1", false},
		{"public, 10.0.0.0/8", "8.8.8.8", true},
		{"ollama.internal", "10.0.0.1", false},
		{"public", "168.63.129.16", false},
		{"::ffff:10.0.0.0/104", "10.1.2.3", true},
		{"::ffff:10.0.0.0/104", "11.0.0.1", false},
		{"::ffff:10.0.0.0/64", "8.8.8.8", false},
		{"::ffff:10.0.0.0/64", "127.0.0.1", false},
		{"::ffff:10.0.0.0/64", "169.254.169.254", false},
	}
	for _, c := range cases {
		err := MustParse(c.policy).CheckAddr("h", netip.MustParseAddr(c.addr))
		if (err == nil) != c.ok {
			t.Errorf("%q allows %s: %v; want ok=%v", c.policy, c.addr, err, c.ok)
		}
		if err != nil && (!errors.Is(err, ErrRefused) || Refusal(err) == nil) {
			t.Errorf("%q refusing %s: %v isn't a refusal", c.policy, c.addr, err)
		}
	}
}

type fakeNet struct {
	server  *httptest.Server
	answers [][]string
	lookups atomic.Int32
	mu      sync.Mutex
	dialed  []string
}

func newFakeNet(t *testing.T, answers ...[]string) *fakeNet {
	f := &fakeNet{answers: answers}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	f.server.Config.SetKeepAlivesEnabled(false)
	f.server.Start()
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeNet) resolve(_ context.Context, _, host string) ([]netip.Addr, error) {
	n := int(f.lookups.Add(1)) - 1
	if n >= len(f.answers) {
		n = len(f.answers) - 1
	}
	var out []netip.Addr
	for _, a := range f.answers[n] {
		out = append(out, netip.MustParseAddr(a))
	}
	if len(out) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return out, nil
}

func (f *fakeNet) dial(ctx context.Context, network, address string) (net.Conn, error) {
	f.mu.Lock()
	f.dialed = append(f.dialed, address)
	f.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, network, f.server.Listener.Addr().String())
}

func (f *fakeNet) dials() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dialed...)
}

func (f *fakeNet) client(p Policy) *http.Client {
	tr := p.Transport(f.resolve)
	tr.DialContext = (&Dialer{Policy: p, Resolve: f.resolve, dial: f.dial}).DialContext
	c := p.Client(5 * time.Second)
	c.Transport = tr
	return c
}

func get(c *http.Client, url string) error {
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

func TestDialerModes(t *testing.T) {
	cases := []struct {
		name, policy, url string
		answer            []string
		wantDial          string
	}{
		{"any allows a private address", "any", "http://models.example:8000/v1", []string{"10.0.0.5"}, "models.example:8000"},
		{"any allows metadata", "any", "http://169.254.169.254/latest", nil, "169.254.169.254:80"},
		{"public allows a public address", "public", "http://models.example/v1", []string{"8.8.8.8"}, "8.8.8.8:80"},
		{"public refuses a private address", "public", "http://models.example/v1", []string{"10.0.0.5"}, ""},
		{"public refuses loopback by name", "public", "http://localhost:11434/v1", []string{"127.0.0.1", "::1"}, ""},
		{"public refuses an IP literal", "public", "http://169.254.169.254/latest", nil, ""},
		{"public refuses an IPv6 literal", "public", "http://[fd00:ec2::254]/latest", nil, ""},
		{"public refuses a mapped literal", "public", "http://[::ffff:a9fe:a9fe]/latest", nil, ""},
		{"public skips a private answer for a public one", "public", "http://models.example/v1", []string{"10.0.0.5", "8.8.8.8"}, "8.8.8.8:80"},
		{"allowlist by name", "ollama.lan", "http://Ollama.LAN:11434/v1", []string{"192.168.1.20"}, "Ollama.LAN:11434"},
		{"allowlist by name refuses others", "ollama.lan", "http://models.example/v1", []string{"192.168.1.20"}, ""},
		{"allowlist by CIDR", "192.168.1.0/24", "http://models.example/v1", []string{"192.168.1.20"}, "192.168.1.20:80"},
		{"allowlist by CIDR refuses outside", "192.168.1.0/24", "http://models.example/v1", []string{"192.168.2.20"}, ""},
		{"allowlist without public refuses public", "10.0.0.0/8", "http://models.example/v1", []string{"8.8.8.8"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeNet(t, c.answer)
			err := get(f.client(MustParse(c.policy)), c.url)
			dials := f.dials()
			if c.wantDial == "" {
				if !errors.Is(err, ErrRefused) || len(dials) != 0 {
					t.Fatalf("err = %v, dials = %v; want a refusal before any connection", err, dials)
				}
				return
			}
			if err != nil || len(dials) != 1 || dials[0] != c.wantDial {
				t.Fatalf("err = %v, dials = %v; want one dial to %s", err, dials, c.wantDial)
			}
		})
	}
}

func TestDNSRebinding(t *testing.T) {
	f := newFakeNet(t, []string{"8.8.8.8"}, []string{"169.254.169.254"})
	c := f.client(MustParse("public"))
	if err := get(c, "http://rebind.example/v1/models"); err != nil {
		t.Fatalf("first request: %v", err)
	}
	err := get(c, "http://rebind.example/v1/models")
	if r := Refusal(err); r == nil || r.Addr != netip.MustParseAddr("169.254.169.254") || r.Reason != "a link-local address" {
		t.Fatalf("second request: err = %v; want a refusal of the link-local address", err)
	}
	if dials := f.dials(); len(dials) != 1 || dials[0] != "8.8.8.8:80" {
		t.Fatalf("dials = %v; want only the public address", dials)
	}
	if f.lookups.Load() != 2 {
		t.Fatalf("lookups = %d; want one per connection", f.lookups.Load())
	}
}

func TestRealConnections(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	url := server.URL + "/v1/models"

	if err := get(Any.Client(5*time.Second), url); err != nil || hits.Load() != 1 {
		t.Fatalf("any: err = %v, hits = %d", err, hits.Load())
	}
	if err := get(MustParse("127.0.0.1").Client(5*time.Second), url); err != nil || hits.Load() != 2 {
		t.Fatalf("allowlisted loopback: err = %v, hits = %d", err, hits.Load())
	}
	for _, policy := range []string{"public", "10.0.0.0/8", "localhost"} {
		err := get(MustParse(policy).Client(5*time.Second), url)
		if !errors.Is(err, ErrRefused) || (policy == "public" && !strings.Contains(err.Error(), "127.0.0.1 is a loopback address")) {
			t.Errorf("%s: err = %v; want a refusal", policy, err)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d; a refused request reached the server", hits.Load())
	}

	d := &Dialer{Policy: MustParse("public")}
	if err := d.control("tcp4", "127.0.0.1:80", nil); !errors.Is(err, ErrRefused) {
		t.Errorf("control(127.0.0.1) = %v; want a refusal", err)
	}
	if err := d.control("tcp4", "8.8.8.8:443", nil); err != nil {
		t.Errorf("control(8.8.8.8) = %v", err)
	}
}

func TestNoRedirects(t *testing.T) {
	var followed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed.Store(true)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer server.Close()
	resp, err := MustParse("127.0.0.1").Client(5 * time.Second).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || followed.Load() {
		t.Fatalf("status = %d, followed = %v; want the redirect itself", resp.StatusCode, followed.Load())
	}
}

func TestCheck(t *testing.T) {
	f := newFakeNet(t, []string{"10.0.0.5", "8.8.8.8"})
	ctx := context.Background()
	if err := MustParse("public").Check(ctx, "models.example", f.resolve); err != nil {
		t.Errorf("a name with a public address: %v", err)
	}
	f = newFakeNet(t, []string{"10.0.0.5"})
	if err := MustParse("public").Check(ctx, "models.example", f.resolve); !errors.Is(err, ErrRefused) ||
		err.Error() != "the server's egress policy doesn't allow this endpoint: models.example resolves to 10.0.0.5, a private address" {
		t.Errorf("a name with only a private address: %v", err)
	}
	if err := MustParse("public").Check(ctx, "[::1]", nil); !errors.Is(err, ErrRefused) {
		t.Errorf("an IPv6 literal: %v", err)
	}
	f = newFakeNet(t, nil)
	if err := MustParse("public").Check(ctx, "nowhere.example", f.resolve); err != nil {
		t.Errorf("a name that doesn't resolve now: %v; want it left to the connection", err)
	}
	if err := MustParse("ollama.lan").Check(ctx, "ollama.lan.", nil); err != nil {
		t.Errorf("a listed name: %v", err)
	}
	if err := Any.Check(ctx, "169.254.169.254", nil); err != nil {
		t.Errorf("any: %v", err)
	}
}
