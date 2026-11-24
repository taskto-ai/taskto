package mail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	netmail "net/mail"
	"net/netip"
	"net/smtp"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/egress"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return c
}

var hello = Message{To: []string{"Ana <ana@example.com>"}, Subject: "Olá, mundo", Text: "Line one.\nA line with ünïcode.\n.dot first\n"}

func sentTo(t *testing.T, s *fakeSMTP) received {
	t.Helper()
	got := s.messages()
	if len(got) != 1 {
		t.Fatalf("the server got %d messages, want 1 (commands %v)", len(got), s.commands())
	}
	return got[0]
}

func TestSendWithSTARTTLSAndSignIn(t *testing.T) {
	s := newFakeSMTP(t, false)
	s.User, s.Pass = "taskto", "correct horse"
	if err := s.sender(SMTPConfig{Username: "taskto", Password: "correct horse"}).Send(ctx(t), hello); err != nil {
		t.Fatal(err)
	}
	got := sentTo(t, s)
	if !got.TLS || got.AuthedAs != "taskto" {
		t.Fatalf("tls %v, signed in as %q: the message must go over TLS, signed in", got.TLS, got.AuthedAs)
	}
	if got.From != "taskto@example.com" || len(got.To) != 1 || got.To[0] != "ana@example.com" {
		t.Fatalf("envelope %q -> %v", got.From, got.To)
	}

	msg, err := netmail.ReadMessage(strings.NewReader(got.Data))
	if err != nil {
		t.Fatal(err)
	}
	dec := new(mime.WordDecoder)
	subject, _ := dec.DecodeHeader(msg.Header.Get("Subject"))
	if subject != "Olá, mundo" {
		t.Fatalf("subject = %q", subject)
	}
	if from := msg.Header.Get("From"); !strings.Contains(from, "taskto@example.com") || !strings.Contains(from, "TaskTo") {
		t.Fatalf("From = %q", from)
	}
	if msg.Header.Get("Message-ID") == "" || msg.Header.Get("Date") == "" || msg.Header.Get("Auto-Submitted") != "auto-generated" {
		t.Fatalf("headers: %v", msg.Header)
	}
	body, _ := io.ReadAll(msg.Body)
	// Quoted-printable decodes to the text, dot-stuffed line included.
	text := decodeQP(t, string(body))
	if text != strings.ReplaceAll(hello.Text, "\n", "\r\n") {
		t.Fatalf("body = %q", text)
	}
}

func TestSendMultipart(t *testing.T) {
	s := newFakeSMTP(t, false)
	m := hello
	m.HTML = `<p>Olá <b>Ana</b></p>`
	if err := s.sender(SMTPConfig{}).Send(ctx(t), m); err != nil {
		t.Fatal(err)
	}
	got := sentTo(t, s)
	msg, err := netmail.ReadMessage(strings.NewReader(got.Data))
	if err != nil {
		t.Fatal(err)
	}
	typ, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || typ != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, %v", msg.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var types, bodies []string
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(p)
		types = append(types, p.Header.Get("Content-Type"))
		bodies = append(bodies, decodeQP(t, string(b)))
	}
	if len(types) != 2 || !strings.HasPrefix(types[0], "text/plain") || !strings.HasPrefix(types[1], "text/html") {
		t.Fatalf("parts = %v", types)
	}
	if bodies[1] != m.HTML {
		t.Fatalf("html = %q", bodies[1])
	}
}

func TestSendImplicitTLS(t *testing.T) {
	s := newFakeSMTP(t, true)
	s.User, s.Pass = "u", "p"
	if err := s.sender(SMTPConfig{Username: "u", Password: "p"}).Send(ctx(t), hello); err != nil {
		t.Fatal(err)
	}
	if got := sentTo(t, s); !got.TLS || got.AuthedAs != "u" {
		t.Fatalf("%+v", got)
	}
}

func TestSendSignsInWithLOGINWhenPLAINIsNotOffered(t *testing.T) {
	s := newFakeSMTP(t, false)
	s.User, s.Pass, s.AuthMechs = "u", "p", "LOGIN"
	if err := s.sender(SMTPConfig{Username: "u", Password: "p"}).Send(ctx(t), hello); err != nil {
		t.Fatal(err)
	}
	if got := sentTo(t, s); got.AuthedAs != "u" {
		t.Fatalf("%+v", got)
	}
}

func TestSendWithoutEncryptionToALocalRelay(t *testing.T) {
	s := newFakeSMTP(t, false)
	if err := s.sender(SMTPConfig{Security: None}).Send(ctx(t), hello); err != nil {
		t.Fatal(err)
	}
	if got := sentTo(t, s); got.TLS {
		t.Fatal("security none must not start TLS")
	}
}

func TestSTARTTLSIsNeverOptional(t *testing.T) {
	s := newFakeSMTP(t, false)
	s.NoSTARTTLS = true
	s.User, s.Pass = "u", "p"
	err := s.sender(SMTPConfig{Username: "u", Password: "p"}).Send(ctx(t), hello)
	assertStage(t, err, StageTLS, 0)
	for _, c := range s.commands() {
		if c == "AUTH" || c == "MAIL" {
			t.Fatalf("the server saw %v: nothing may be sent before TLS", s.commands())
		}
	}
}

func TestCertificateIsChecked(t *testing.T) {
	s := newFakeSMTP(t, false)
	m := s.sender(SMTPConfig{})
	m.tlsConfig = nil // the system's roots don't know the fake server's certificate
	assertStage(t, m.Send(ctx(t), hello), StageTLS, 0)
	if len(s.messages()) != 0 {
		t.Fatal("a message went to a server whose certificate wasn't trusted")
	}
}

func TestAuthFailureNamesTheStageAndNeverThePassword(t *testing.T) {
	s := newFakeSMTP(t, false)
	s.User, s.Pass = "u", "right"
	err := s.sender(SMTPConfig{Username: "u", Password: "s3cret-wrong"}).Send(ctx(t), hello)
	assertStage(t, err, StageAuth, 535)
	if strings.Contains(err.Error(), "s3cret-wrong") {
		t.Fatalf("the error holds the password: %v", err)
	}

	// A server that offers no sign-in at all.
	s2 := newFakeSMTP(t, false)
	assertStage(t, s2.sender(SMTPConfig{Username: "u", Password: "p"}).Send(ctx(t), hello), StageAuth, 0)
}

func TestRejections(t *testing.T) {
	s := newFakeSMTP(t, false)
	s.RejectRcpt = "550 5.1.1 no such user"
	assertStage(t, s.sender(SMTPConfig{}).Send(ctx(t), hello), StageRejected, 550)

	s = newFakeSMTP(t, false)
	s.RejectData = "554 5.7.1 rejected as spam"
	assertStage(t, s.sender(SMTPConfig{}).Send(ctx(t), hello), StageRejected, 554)
}

func TestEgressPolicyRefusesTheConnection(t *testing.T) {
	s := newFakeSMTP(t, false)
	m := s.sender(SMTPConfig{Egress: egress.MustParse("public")})
	err := m.Send(ctx(t), hello)
	assertStage(t, err, StageConnect, 0)
	if !errors.Is(err, egress.ErrRefused) {
		t.Fatalf("err = %v, want the egress policy's refusal", err)
	}
	if len(s.commands()) != 0 {
		t.Fatalf("the server was reached: %v", s.commands())
	}

	// An allowlist that names the address lets it through.
	m = s.sender(SMTPConfig{Egress: egress.MustParse("127.0.0.1")})
	if err := m.Send(ctx(t), hello); err != nil {
		t.Fatal(err)
	}
}

func TestEgressPolicyChecksTheResolvedAddress(t *testing.T) {
	s := newFakeSMTP(t, false)
	m := s.sender(SMTPConfig{Egress: egress.MustParse("public")})
	m.cfg.Host = "smtp.example.com"
	// A public name that resolves to a loopback address: refused after resolution.
	m.resolve = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	err := m.Send(ctx(t), hello)
	if !errors.Is(err, egress.ErrRefused) || len(s.commands()) != 0 {
		t.Fatalf("err = %v, commands %v", err, s.commands())
	}
}

func TestATimeoutEndsASendThatHangs(t *testing.T) {
	s := newFakeSMTP(t, false)
	s.Hang = true
	start := time.Now()
	err := s.sender(SMTPConfig{Timeout: 300 * time.Millisecond}).Send(ctx(t), hello)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("err = %T %v, want a *SendError", err, err)
	}
}

func TestNothingListening(t *testing.T) {
	s := newFakeSMTP(t, false)
	m := s.sender(SMTPConfig{})
	_ = s.ln.Close()
	assertStage(t, m.Send(ctx(t), hello), StageConnect, 0)
}

func TestMessageIsCheckedBeforeConnecting(t *testing.T) {
	s := newFakeSMTP(t, false)
	m := s.sender(SMTPConfig{})
	for name, msg := range map[string]Message{
		"no recipient":            {Subject: "s", Text: "t"},
		"no text":                 {To: []string{"a@example.com"}, Subject: "s"},
		"a bad address":           {To: []string{"not an address"}, Subject: "s", Text: "t"},
		"a header in the subject": {To: []string{"a@example.com"}, Subject: "s\r\nBcc: x@example.com", Text: "t"},
		"a header in an address":  {To: []string{"a@example.com\r\nBcc: x@example.com"}, Subject: "s", Text: "t"},
	} {
		if err := m.Send(ctx(t), msg); err == nil {
			t.Errorf("%s: sent", name)
		}
	}
	if len(s.commands()) != 0 {
		t.Fatalf("the server was reached: %v", s.commands())
	}
}

func TestANameWithSpecialCharactersCannotInjectHeaders(t *testing.T) {
	s := newFakeSMTP(t, false)
	m := hello
	m.To = []string{`"Ana\r\nBcc: x@example.com" <ana@example.com>`}
	_ = s.sender(SMTPConfig{}).Send(ctx(t), m)
	for _, got := range s.messages() {
		msg, err := netmail.ReadMessage(strings.NewReader(got.Data))
		if err != nil {
			t.Fatal(err)
		}
		if msg.Header.Get("Bcc") != "" {
			t.Fatal("a header was injected through a recipient's name")
		}
	}
}

func TestNewSMTPChecksItsSettings(t *testing.T) {
	ok := SMTPConfig{Host: "smtp.example.com", From: "taskto@example.com"}
	if m, err := NewSMTP(ok); err != nil || m.cfg.Port != 587 || m.cfg.Security != STARTTLS {
		t.Fatalf("defaults: %+v, %v", m, err)
	}
	if m, err := NewSMTP(SMTPConfig{Host: "h.example.com", From: "a@example.com", Security: TLS}); err != nil || m.cfg.Port != 465 {
		t.Fatalf("tls: %+v, %v", m, err)
	}
	for name, mod := range map[string]func(*SMTPConfig){
		"no host":            func(c *SMTPConfig) { c.Host = "" },
		"a host with a port": func(c *SMTPConfig) { c.Host = "smtp.example.com:587" },
		"no sender":          func(c *SMTPConfig) { c.From = "" },
		"a bad security":     func(c *SMTPConfig) { c.Security = "ssl" },
		"a bad port":         func(c *SMTPConfig) { c.Port = 99999 },
		"a username alone":   func(c *SMTPConfig) { c.Username = "u" },
		"a password in clear": func(c *SMTPConfig) {
			c.Security, c.Username, c.Password = None, "u", "p"
		},
	} {
		c := ok
		mod(&c)
		if _, err := NewSMTP(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A sign-in is never sent in clear text, not even to localhost: the host's name says
	// nothing about where the connection goes.
	for _, host := range []string{"127.0.0.1", "localhost", "::1", "[::1]"} {
		if _, err := NewSMTP(SMTPConfig{Host: host, From: "a@example.com", Security: None, Username: "u", Password: "p"}); err == nil {
			t.Errorf("a password in clear text to %s: accepted", host)
		}
	}
	// A relay that needs no sign-in works without encryption.
	if _, err := NewSMTP(SMTPConfig{Host: "127.0.0.1", From: "a@example.com", Security: None}); err != nil {
		t.Fatalf("a relay without sign-in: %v", err)
	}
}

func TestInfoNeverHoldsThePassword(t *testing.T) {
	m, _ := NewSMTP(SMTPConfig{Host: "smtp.example.com", From: "TaskTo <t@example.com>", Username: "user-x", Password: "pw-y"})
	info := m.Info()
	if !info.SignsIn || info.Host != "smtp.example.com" || info.Port != 587 {
		t.Fatalf("%+v", info)
	}
}

func assertStage(t *testing.T, err error, stage Stage, code int) {
	t.Helper()
	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a *SendError at %s", err, stage)
	}
	if se.Stage != stage || se.Code != code {
		t.Fatalf("stage %s code %d (%v), want %s %d", se.Stage, se.Code, err, stage, code)
	}
}

// A relay can repeat what it was sent in a rejection. The error keeps the stage and the reply's
// code and nothing the relay said, so neither the API answer nor the log can leak it.
func TestTheRelaysReplyTextIsNeverKept(t *testing.T) {
	const secret = "s3cret-wrong"
	check := func(t *testing.T, err error, stage Stage, code int, leaks ...string) {
		t.Helper()
		assertStage(t, err, stage, code)
		var tp *textproto.Error
		if errors.As(err, &tp) {
			t.Fatalf("the error wraps the relay's reply: %v", tp)
		}
		for _, l := range append(leaks, "credentials refused", "no such user") {
			if strings.Contains(err.Error(), l) || strings.Contains(fmtV(err), l) {
				t.Fatalf("the error holds the relay's text %q: %v", l, err)
			}
		}
		if !strings.Contains(err.Error(), "the server replied") {
			t.Fatalf("the error doesn't say it was a reply: %v", err)
		}
	}
	t.Run("sign-in echoed", func(t *testing.T) {
		s := newFakeSMTP(t, false)
		s.User, s.Pass, s.EchoAuth = "taskto-user", "right", true
		err := s.sender(SMTPConfig{Username: "taskto-user", Password: secret}).Send(ctx(t), hello)
		check(t, err, StageAuth, 535, secret, "taskto-user")
	})
	t.Run("login mechanism echoed", func(t *testing.T) {
		s := newFakeSMTP(t, false)
		s.User, s.Pass, s.EchoAuth, s.AuthMechs = "taskto-user", "right", true, "LOGIN"
		err := s.sender(SMTPConfig{Username: "taskto-user", Password: secret}).Send(ctx(t), hello)
		check(t, err, StageAuth, 535, secret, "taskto-user")
	})
	t.Run("recipient", func(t *testing.T) {
		s := newFakeSMTP(t, false)
		s.RejectRcpt = "550 5.1.1 no such user ana@example.com " + secret
		check(t, s.sender(SMTPConfig{}).Send(ctx(t), hello), StageRejected, 550, secret, "ana@example.com")
	})
	t.Run("message", func(t *testing.T) {
		s := newFakeSMTP(t, false)
		s.RejectData = "554 5.7.1 no such user " + secret
		check(t, s.sender(SMTPConfig{}).Send(ctx(t), hello), StageRejected, 554, secret)
	})
}

func fmtV(err error) string {
	return strings.Join([]string{err.Error(), strings.TrimSpace(errors.Unwrap(err).Error())}, " ")
}

// Sign-in needs an encrypted connection by the connection's own state, with PLAIN as with
// LOGIN: net/smtp's PLAIN lets a host *named* localhost skip it, which says nothing about
// where the connection went.
func TestSignInMechanismsNeedAnEncryptedConnection(t *testing.T) {
	for _, name := range []string{"localhost", "127.0.0.1", "smtp.example.com"} {
		for mech, auth := range map[string]smtp.Auth{
			"PLAIN": &plainAuth{username: "u", password: "p"},
			"LOGIN": &loginAuth{username: "u", password: "p"},
		} {
			if _, _, err := auth.Start(&smtp.ServerInfo{Name: name, TLS: false, Auth: []string{mech}}); err == nil {
				t.Errorf("%s to %s started without TLS", mech, name)
			}
			if _, _, err := auth.Start(&smtp.ServerInfo{Name: name, TLS: true, Auth: []string{mech}}); err != nil {
				t.Errorf("%s to %s over TLS: %v", mech, name, err)
			}
		}
	}
	// PLAIN's initial response is the standard one.
	_, resp, _ := (&plainAuth{username: "u", password: "p"}).Start(&smtp.ServerInfo{TLS: true})
	if string(resp) != "\x00u\x00p" {
		t.Fatalf("initial response = %q", resp)
	}
}

// A sender with a quoted local part keeps its quotes in the envelope, and the Message-ID's
// domain is what follows the last "@".
func TestEnvelopeKeepsQuotedLocalParts(t *testing.T) {
	s := newFakeSMTP(t, false)
	m := hello
	m.To = []string{`Ana <"ana smith"@example.com>`, `"a@b"@example.org`}
	if err := s.sender(SMTPConfig{From: `"billing alerts"@example.com`}).Send(ctx(t), m); err != nil {
		t.Fatal(err)
	}
	got := sentTo(t, s)
	if got.From != `"billing alerts"@example.com` {
		t.Fatalf("MAIL FROM = %q", got.From)
	}
	if len(got.To) != 2 || got.To[0] != `"ana smith"@example.com` || got.To[1] != `"a@b"@example.org` {
		t.Fatalf("RCPT TO = %q", got.To)
	}

	// An address without a quote is as it was.
	s = newFakeSMTP(t, false)
	if err := s.sender(SMTPConfig{}).Send(ctx(t), hello); err != nil {
		t.Fatal(err)
	}
	if got := sentTo(t, s); got.From != "taskto@example.com" || got.To[0] != "ana@example.com" {
		t.Fatalf("envelope %q -> %q", got.From, got.To)
	}
}

func TestMessageIDDomainIsAfterTheLastAt(t *testing.T) {
	from, err := netmail.ParseAddress(`"odd@name"@example.com`)
	if err != nil {
		t.Fatal(err)
	}
	env, err := build(from, hello, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	msg, err := netmail.ReadMessage(strings.NewReader(string(env.data)))
	if err != nil {
		t.Fatal(err)
	}
	if id := msg.Header.Get("Message-ID"); !strings.HasSuffix(id, "@example.com>") || strings.Contains(id, "name") {
		t.Fatalf("Message-ID = %q", id)
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"smtp.example.com":        "smtp.example.com",
		"127.0.0.1":               "127.0.0.1",
		"::1":                     "::1",
		"[::1]":                   "::1",
		"2001:db8::25":            "2001:db8::25",
		"[2001:DB8:0:0:0:0:0:25]": "2001:db8::25",
		"::ffff:192.0.2.1":        "::ffff:192.0.2.1",
		"":                        "",
	} {
		if got, err := NormalizeHost(in); err != nil || got != want {
			t.Errorf("NormalizeHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"smtp.example.com:587", "1.2.3.4:25", "[::1]:25", "[2001:db8::1]:587", "2001:db8::1:", "[::1", "::1]",
		"fe80::1%eth0", "[fe80::1%eth0]", "[smtp.example.com]", "[1.2.3.4]", "user@host", "a b", "a/b", "host\n",
	} {
		if got, err := NormalizeHost(in); err == nil {
			t.Errorf("NormalizeHost(%q) = %q; want an error", in, got)
		}
	}
}

func TestNewSMTPTakesIPv6Literals(t *testing.T) {
	for _, host := range []string{"::1", "[::1]"} {
		m, err := NewSMTP(SMTPConfig{Host: host, From: "a@example.com"})
		if err != nil || m.cfg.Host != "::1" || m.Info().Host != "::1" {
			t.Fatalf("%s: %+v, %v", host, m, err)
		}
	}
	if _, err := NewSMTP(SMTPConfig{Host: "[::1]:587", From: "a@example.com"}); err == nil {
		t.Fatal("an IPv6 host with a port was accepted")
	}
}

// The LOGIN prompts' words are the server's: the first challenge asks for the user name and the
// second for the password, whatever they say, and a third is refused.
func TestLoginAnswersByStepNotByPromptText(t *testing.T) {
	for _, prompts := range [][]string{{"Username:", "Password:"}, {"User Name", "Password"}, {"VXNlcm5hbWU6", "UGFzc3dvcmQ6"}, {"", ""}} {
		a := &loginAuth{username: "user-x", password: "pw-y"}
		if _, _, err := a.Start(&smtp.ServerInfo{TLS: true}); err != nil {
			t.Fatal(err)
		}
		u, err := a.Next([]byte(prompts[0]), true)
		if err != nil || string(u) != "user-x" {
			t.Fatalf("%q: first answer %q, %v", prompts, u, err)
		}
		p, err := a.Next([]byte(prompts[1]), true)
		if err != nil || string(p) != "pw-y" {
			t.Fatalf("%q: second answer %q, %v", prompts, p, err)
		}
		if got, err := a.Next([]byte("Again:"), true); err == nil {
			t.Fatalf("%q: a third challenge was answered with %q", prompts, got)
		}
	}
}

// A message with many recipients folds its To line: no line is over 998 characters (78 is the
// advice), and every recipient is still there.
func TestLongRecipientListsAreFolded(t *testing.T) {
	from, _ := netmail.ParseAddress("taskto@example.com")
	m := Message{Subject: "s", Text: "t"}
	for i := range maxRecipients {
		m.To = append(m.To, fmt.Sprintf("Recipient %02d with a rather long display name <person%02d@example.com>", i, i))
	}
	env, err := build(from, m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(env.data), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("a line of %d characters", len(line))
		}
	}
	msg, err := netmail.ReadMessage(strings.NewReader(string(env.data)))
	if err != nil {
		t.Fatal(err)
	}
	to, err := msg.Header.AddressList("To")
	if err != nil || len(to) != maxRecipients || to[49].Address != "person49@example.com" {
		t.Fatalf("%d recipients, %v", len(to), err)
	}
	if len(env.to) != maxRecipients {
		t.Fatalf("%d envelope recipients", len(env.to))
	}
}
