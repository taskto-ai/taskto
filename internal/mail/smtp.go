package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	netmail "net/mail"
	"net/netip"
	"net/smtp"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"time"

	"taskto.ai/taskto/internal/egress"
)

// Security says how an SMTP connection is protected.
type Security string

const (
	// STARTTLS connects in clear text and upgrades before anything private is sent. A server
	// that doesn't offer it is refused: the upgrade is never optional.
	STARTTLS Security = "starttls"
	// TLS is implicit TLS from the first byte (port 465).
	TLS Security = "tls"
	// None is no encryption, for a trusted relay on the same host or network that needs no
	// sign-in: a username and password are never sent this way, to any address.
	None Security = "none"
)

// DefaultTimeout bounds a whole send, connecting included.
const DefaultTimeout = 30 * time.Second

// SMTPConfig is how to reach and sign in to an SMTP server.
type SMTPConfig struct {
	Host string
	// Port is the server's port; 0 is the security's usual one.
	Port     int
	Security Security
	// From is the sender of every message: an address, or "Name <address>".
	From string
	// Username and Password sign in; both empty is no sign-in.
	Username string
	Password string
	// Egress limits where the connection may go. The zero policy allows any address.
	Egress egress.Policy
	// Hello is the name the server announces itself as in EHLO; empty is "localhost".
	Hello string
	// Timeout bounds a send; zero is DefaultTimeout.
	Timeout time.Duration
}

// SMTP sends messages through an SMTP server, one connection per message: the server's mail
// is occasional, and a connection that is never idle never goes stale.
type SMTP struct {
	cfg  SMTPConfig
	from *netmail.Address
	// tlsConfig is the TLS settings to build on; tests replace it to trust their own
	// certificate. nil means the system's roots.
	tlsConfig *tls.Config
	// resolve looks names up; nil is the system's. Tests replace it.
	resolve egress.Resolver
}

// NewSMTP checks cfg and returns an SMTP sender. It connects to nothing.
func NewSMTP(cfg SMTPConfig) (*SMTP, error) {
	host, err := NormalizeHost(cfg.Host)
	if err != nil || host == "" {
		return nil, errors.New("mail: the SMTP host is a name or an address, without a port")
	}
	cfg.Host = host
	switch cfg.Security {
	case "":
		cfg.Security = STARTTLS
	case STARTTLS, TLS, None:
	default:
		return nil, fmt.Errorf("mail: unknown security %q", cfg.Security)
	}
	if cfg.Port == 0 {
		cfg.Port = map[Security]int{STARTTLS: 587, TLS: 465, None: 25}[cfg.Security]
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, errors.New("mail: the SMTP port is 1 to 65535")
	}
	from, err := netmail.ParseAddress(cfg.From)
	if err != nil {
		return nil, errors.New("mail: the sender isn't an email address")
	}
	if (cfg.Username == "") != (cfg.Password == "") {
		return nil, errors.New("mail: the SMTP username and password go together")
	}
	if cfg.Username != "" && cfg.Security == None {
		return nil, errors.New("mail: a password isn't sent without encryption: use starttls or tls")
	}
	if cfg.Hello == "" {
		cfg.Hello = "localhost"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	return &SMTP{cfg: cfg, from: from}, nil
}

// Info is what admins are told of s: nothing secret.
func (s *SMTP) Info() Info {
	return Info{Host: s.cfg.Host, Port: s.cfg.Port, Security: string(s.cfg.Security), From: s.from.String(),
		SignsIn: s.cfg.Password != ""}
}

// Send sends m through the server.
func (s *SMTP) Send(ctx context.Context, m Message) error {
	env, err := build(s.from, m, time.Now())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	conn, err := s.connect(ctx)
	if err != nil {
		return err
	}
	// Whatever happens next, the context ending closes the connection, which ends any read
	// or write that waits on the server.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return failed(ctx, StageConnect, err)
	}
	defer c.Close()
	if err := c.Hello(s.cfg.Hello); err != nil {
		return failed(ctx, StageSend, err)
	}
	if s.cfg.Security == STARTTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return &SendError{Stage: StageTLS, Err: errors.New("the server doesn't offer STARTTLS")}
		}
		if err := c.StartTLS(s.tlsFor()); err != nil {
			return failed(ctx, StageTLS, err)
		}
	}
	if s.cfg.Username != "" {
		if err := s.authenticate(c); err != nil {
			return failed(ctx, StageAuth, err)
		}
	}
	if err := c.Mail(env.from); err != nil {
		return failed(ctx, StageRejected, err)
	}
	for _, to := range env.to {
		if err := c.Rcpt(to); err != nil {
			return failed(ctx, StageRejected, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return failed(ctx, StageRejected, err)
	}
	if _, err := w.Write(env.data); err != nil {
		return failed(ctx, StageSend, err)
	}
	if err := w.Close(); err != nil {
		// The server's answer to the message: the last word on whether it was taken. Without a
		// reply (the connection dropped, the time ran out) there is no word, and it may have been.
		e := failed(ctx, StageRejected, err)
		e.Ambiguous = e.Code == 0
		return e
	}
	// The message is accepted; a server that drops the connection on QUIT hasn't undone it.
	_ = c.Quit()
	return nil
}

// connect opens the connection, through the egress policy, with TLS already on for the
// implicit kind.
func (s *SMTP) connect(ctx context.Context) (net.Conn, error) {
	d := &egress.Dialer{Policy: s.cfg.Egress, Resolve: s.resolve}
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, failed(ctx, StageConnect, err)
	}
	if s.cfg.Security != TLS {
		return conn, nil
	}
	tc := tls.Client(conn, s.tlsFor())
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, failed(ctx, StageTLS, err)
	}
	return tc, nil
}

func (s *SMTP) tlsFor() *tls.Config {
	var c *tls.Config
	if s.tlsConfig != nil {
		c = s.tlsConfig.Clone()
	} else {
		c = &tls.Config{}
	}
	c.ServerName = s.cfg.Host
	c.MinVersion = tls.VersionTLS12
	return c
}

// authenticate signs in with the best mechanism the server offers: PLAIN, else LOGIN. Both
// send the password only on a connection that is encrypted now, whatever the host's name.
func (s *SMTP) authenticate(c *smtp.Client) error {
	ok, offered := c.Extension("AUTH")
	if !ok {
		return errors.New("the server doesn't offer sign-in")
	}
	mechs := strings.Fields(strings.ToUpper(offered))
	switch {
	case slices.Contains(mechs, "PLAIN"):
		return c.Auth(&plainAuth{username: s.cfg.Username, password: s.cfg.Password})
	case slices.Contains(mechs, "LOGIN"):
		return c.Auth(&loginAuth{username: s.cfg.Username, password: s.cfg.Password})
	}
	return errors.New("the server offers no sign-in this server speaks (PLAIN or LOGIN)")
}

// errClearText is why a mechanism refuses to start: the connection isn't encrypted. net/smtp's
// own PLAIN allows clear text to a host that is *named* localhost, which says nothing about
// where the connection went; these never do.
var errClearText = errors.New("refusing to send the password without encryption")

// plainAuth is the PLAIN mechanism, which needs the connection to be encrypted.
type plainAuth struct {
	username, password string
}

func (a *plainAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errClearText
	}
	return "PLAIN", []byte("\x00" + a.username + "\x00" + a.password), nil
}

func (a *plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected challenge from the server")
	}
	return nil, nil
}

// loginAuth is the LOGIN mechanism, which some servers (Microsoft 365 among them) offer in
// place of PLAIN. It needs the connection to be encrypted too. The server's prompts aren't
// read ("Username:", "User Name" and others all occur): the first challenge is answered with
// the user name, the second with the password, and a third is an error.
type loginAuth struct {
	username, password string
	answered           int
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errClearText
	}
	a.answered = 0
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.answered++
	switch a.answered {
	case 1:
		return []byte(a.username), nil
	case 2:
		return []byte(a.password), nil
	}
	return nil, errors.New("unexpected challenge from the server")
}

// failed classifies err as a *SendError at stage. A server's reply keeps its code and nothing
// else: its text can echo what was sent (a relay may repeat the sign-in in a rejection), so it
// is dropped here, before the error is returned or logged. A failure that isn't a reply's is
// moved to stage; the context ending is a connect or send failure of its own.
func failed(ctx context.Context, stage Stage, err error) *SendError {
	e := &SendError{Stage: stage, Err: err}
	var tp *textproto.Error
	if errors.As(err, &tp) {
		e.Code = tp.Code
		e.Err = fmt.Errorf("the server replied %d", tp.Code)
		return e
	}
	// A reply that isn't one (no code, or a code and no space) comes back with its whole line.
	var pe textproto.ProtocolError
	if errors.As(err, &pe) {
		e.Err = errors.New("the server's reply was malformed")
		return e
	}
	if ctx.Err() != nil {
		e.Err = fmt.Errorf("%w (%w)", err, ctx.Err())
	}
	return e
}

// NormalizeHost checks an SMTP server's host and returns it as the connection wants it: a
// name or an address, without a port. An IPv6 address, bracketed or not, comes back without
// brackets (they are added where the address and the port are joined). A zone ("%eth0") or
// anything that makes a port, a user or a path is refused. "" is "".
func NormalizeHost(host string) (string, error) {
	if host == "" {
		return "", nil
	}
	inner, bracketed := strings.CutPrefix(host, "[")
	if bracketed {
		var ok bool
		if inner, ok = strings.CutSuffix(inner, "]"); !ok {
			return "", errors.New("a bracket that isn't closed")
		}
	}
	if bracketed || strings.Contains(host, ":") {
		a, err := netip.ParseAddr(inner)
		if err != nil || !a.Is6() || a.Zone() != "" {
			return "", errors.New("a colon is only for an IPv6 address, and the port is its own setting")
		}
		return a.String(), nil
	}
	if strings.ContainsAny(host, " \t\r\n/@[]") {
		return "", errors.New("not a host name or an address")
	}
	return host, nil
}
