package mail

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	netmail "net/mail"
	"net/netip"
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
	if _, err := NewSMTP(SMTPConfig{Host: "127.0.0.1", From: "a@example.com", Security: None, Username: "u", Password: "p"}); err != nil {
		t.Fatalf("a password to a loopback relay: %v", err)
	}
}

func TestInfoNeverHoldsThePassword(t *testing.T) {
	m, _ := NewSMTP(SMTPConfig{Host: "smtp.example.com", From: "TaskTo <t@example.com>", Username: "user-x", Password: "pw-y"})
	info := m.Info()
	if !info.PasswordSet || info.Host != "smtp.example.com" || info.Port != 587 {
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
