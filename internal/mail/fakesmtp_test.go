package mail

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSMTP struct {
	t        *testing.T
	ln       net.Listener
	cert     tls.Certificate
	roots    *x509.CertPool
	implicit bool

	NoSTARTTLS    bool
	AuthMechs     string
	User, Pass    string
	RejectRcpt    string
	RejectData    string
	DropAfterData bool
	Hang          bool
	MalformedAuth bool
	EchoAuth      bool

	start    sync.Once
	mu       sync.Mutex
	received []received
	cmds     []string
}

type received struct {
	From     string
	To       []string
	Data     string
	TLS      bool
	AuthedAs string
}

func newFakeSMTP(t *testing.T, implicitTLS bool) *fakeSMTP {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake smtp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	roots := x509.NewCertPool()
	roots.AddCert(leaf)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{t: t, ln: ln, cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots: roots,
		implicit: implicitTLS, AuthMechs: "PLAIN LOGIN"}
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeSMTP) host() string { return "127.0.0.1" }
func (s *fakeSMTP) port() int    { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *fakeSMTP) sender(cfg SMTPConfig) *SMTP {
	s.t.Helper()
	s.start.Do(func() { go s.serve() })
	cfg.Host, cfg.Port = s.host(), s.port()
	if cfg.From == "" {
		cfg.From = "taskto <taskto@example.com>"
	}
	if cfg.Security == "" && s.implicit {
		cfg.Security = TLS
	}
	m, err := NewSMTP(cfg)
	if err != nil {
		s.t.Fatal(err)
	}
	m.tlsConfig = &tls.Config{RootCAs: s.roots}
	return m
}

func (s *fakeSMTP) messages() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received(nil), s.received...)
}

func (s *fakeSMTP) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

func (s *fakeSMTP) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSMTP) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if s.Hang {
		time.Sleep(5 * time.Second)
		return
	}
	isTLS := false
	if s.implicit {
		tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{s.cert}})
		if tc.Handshake() != nil {
			return
		}
		conn, isTLS = tc, true
	}
	r, w := bufio.NewReader(conn), conn
	reply := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\r\n", a...) }
	line := func() (string, bool) {
		l, err := r.ReadString('\n')
		return strings.TrimRight(l, "\r\n"), err == nil
	}
	reply("220 fake.example.com ESMTP")

	var cur received
	authed := ""
	for {
		l, ok := line()
		if !ok {
			return
		}
		verb, arg, _ := strings.Cut(l, " ")
		verb = strings.ToUpper(verb)
		s.mu.Lock()
		s.cmds = append(s.cmds, verb)
		s.mu.Unlock()
		switch verb {
		case "EHLO", "HELO":
			reply("250-fake.example.com")
			if !isTLS && !s.NoSTARTTLS && !s.implicit {
				reply("250-STARTTLS")
			}
			if s.AuthMechs != "-" && s.User != "" {
				reply("250-AUTH %s", s.AuthMechs)
			}
			reply("250 8BITMIME")
		case "STARTTLS":
			reply("220 ready")
			tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{s.cert}})
			if tc.Handshake() != nil {
				return
			}
			conn, isTLS = tc, true
			r, w = bufio.NewReader(conn), conn
			reply = func(format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\r\n", a...) }
		case "AUTH":
			mech, initial, _ := strings.Cut(arg, " ")
			var user, pass string
			switch strings.ToUpper(mech) {
			case "PLAIN":
				if initial == "" {
					reply("334 ")
					initial, _ = line()
				}
				raw, _ := base64.StdEncoding.DecodeString(initial)
				parts := strings.Split(string(raw), "\x00")
				if len(parts) == 3 {
					user, pass = parts[1], parts[2]
				}
			case "LOGIN":
				reply("334 %s", base64.StdEncoding.EncodeToString([]byte("Username:")))
				u, _ := line()
				reply("334 %s", base64.StdEncoding.EncodeToString([]byte("Password:")))
				p, _ := line()
				ub, _ := base64.StdEncoding.DecodeString(u)
				pb, _ := base64.StdEncoding.DecodeString(p)
				user, pass = string(ub), string(pb)
			default:
				reply("504 5.5.4 unsupported")
				continue
			}
			if s.User != "" && user == s.User && pass == s.Pass {
				authed = user
				reply("235 2.7.0 welcome")
			} else {
				if s.MalformedAuth {
					reply("535x password=%s", pass)
				} else if s.EchoAuth {
					reply("535 5.7.8 credentials refused for %s with password %s", user, pass)
				} else {
					reply("535 5.7.8 credentials refused")
				}
			}
		case "MAIL":
			cur = received{TLS: isTLS, AuthedAs: authed}
			cur.From = pathOf(strings.TrimPrefix(arg, "FROM:"))
			reply("250 2.1.0 ok")
		case "RCPT":
			if s.RejectRcpt != "" {
				reply("%s", s.RejectRcpt)
				continue
			}
			to := pathOf(strings.TrimPrefix(arg, "TO:"))
			cur.To = append(cur.To, to)
			reply("250 2.1.5 ok")
		case "DATA":
			reply("354 go ahead")
			var data strings.Builder
			for {
				l, ok := line()
				if !ok {
					return
				}
				if l == "." {
					break
				}
				data.WriteString(strings.TrimPrefix(l, ".") + "\r\n")
			}
			cur.Data = data.String()
			if s.DropAfterData {
				s.mu.Lock()
				s.received = append(s.received, cur)
				s.mu.Unlock()
				return
			}
			if s.RejectData != "" {
				reply("%s", s.RejectData)
				continue
			}
			s.mu.Lock()
			s.received = append(s.received, cur)
			n := len(s.received)
			s.mu.Unlock()
			reply("250 2.0.0 queued as %s", strconv.Itoa(n))
		case "RSET", "NOOP":
			reply("250 ok")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 5.5.2 unknown")
		}
	}
}

func pathOf(arg string) string {
	i, j := strings.Index(arg, "<"), strings.LastIndex(arg, ">")
	if i < 0 || j < i {
		return arg
	}
	return arg[i+1 : j]
}
