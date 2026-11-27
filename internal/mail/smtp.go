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

type Security string

const (
	STARTTLS Security = "starttls"
	TLS      Security = "tls"
	None     Security = "none"
)

const DefaultTimeout = 30 * time.Second

type SMTPConfig struct {
	Host     string
	Port     int
	Security Security
	From     string
	Username string
	Password string
	Egress   egress.Policy
	Hello    string
	Timeout  time.Duration
}

type SMTP struct {
	cfg       SMTPConfig
	from      *netmail.Address
	tlsConfig *tls.Config
	resolve   egress.Resolver
}

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

func (s *SMTP) Info() Info {
	return Info{Host: s.cfg.Host, Port: s.cfg.Port, Security: string(s.cfg.Security), From: s.from.String(),
		SignsIn: s.cfg.Password != ""}
}

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
		e := failed(ctx, StageRejected, err)
		e.Ambiguous = e.Code == 0
		return e
	}
	_ = c.Quit()
	return nil
}

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

var errClearText = errors.New("refusing to send the password without encryption")

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

func failed(ctx context.Context, stage Stage, err error) *SendError {
	e := &SendError{Stage: stage, Err: err}
	var tp *textproto.Error
	if errors.As(err, &tp) {
		e.Code = tp.Code
		e.Err = fmt.Errorf("the server replied %d", tp.Code)
		return e
	}
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
