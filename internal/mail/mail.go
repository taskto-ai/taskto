// Package mail sends the server's outgoing email: a small interface, an SMTP implementation
// on the standard library, and the translated templates a message is made from.
//
// The operator configures one SMTP server for the whole instance (docs/install.md#email),
// whether it serves one person or a company; nothing here depends on workspaces. The cards
// that use it (password resets, email invitations, notifications) build a message from the
// catalog in the recipient's language (Catalog.Render) and hand it to a Service.
//
// The connection goes through the egress policy (internal/egress), so an operator can keep
// the server from reaching anywhere but their relay. The password is read from a file at
// startup, lives only in memory and is never logged, returned or put in an error.
package mail

import (
	"context"
	"errors"
	"fmt"
)

// Message is one email to send: a subject and a plain text body, and optionally the same
// content as simple HTML, which makes the message multipart/alternative.
type Message struct {
	// To are the recipients: each an address ("ana@example.com") or "Name <address>".
	To []string
	// Subject is one line.
	Subject string
	// Text is the plain text body, always present.
	Text string
	// HTML is the same content as simple HTML, or empty.
	HTML string
}

// Sender sends messages. SMTP is the implementation; tests give their own.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// ErrNotConfigured is returned when the operator hasn't set up an SMTP server.
var ErrNotConfigured = errors.New("mail: outgoing email isn't configured")

// Stage is where a send failed.
type Stage string

const (
	// StageConnect: the server wasn't reached (or the egress policy refused the address).
	StageConnect Stage = "connect"
	// StageTLS: the TLS handshake failed, or the server doesn't offer STARTTLS.
	StageTLS Stage = "tls"
	// StageAuth: the server refused the sign-in, or offers no way to sign in that we speak.
	StageAuth Stage = "auth"
	// StageRejected: the server refused the sender, a recipient or the message.
	StageRejected Stage = "rejected"
	// StageSend: any other failure while talking to the server.
	StageSend Stage = "send"
)

// SendError says where a send failed and what the server answered. Its text can include
// the server's own reply, so it belongs in the log, not in an API answer; Stage and Code
// are what people are told. It never contains the password.
type SendError struct {
	Stage Stage
	// Code is the server's SMTP reply code (such as 550), or 0.
	Code int
	Err  error
	// Ambiguous is set when the whole message was sent and the server's answer to it never came
	// (the connection was lost, or the time ran out, after the end of the data): the server may
	// have accepted it. Every other failure is a definite "not accepted", so sending again can't
	// make a second copy; after an ambiguous one it can.
	Ambiguous bool
}

func (e *SendError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("mail: %s: %d: %v", e.Stage, e.Code, e.Err)
	}
	return fmt.Sprintf("mail: %s: %v", e.Stage, e.Err)
}

func (e *SendError) Unwrap() error { return e.Err }

// Info describes the configured server, for the admins' settings page: nothing secret.
type Info struct {
	// Host, Port and Security say where and how the server connects.
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"`
	// From is the sender of every message.
	From string `json:"from"`
	// SignsIn is whether the server signs in to the SMTP server (the name says nothing of the
	// credentials, which Info never holds).
	SignsIn bool `json:"-"`
}

// Service is the server's outgoing email: a Sender and what to tell admins about it. The
// zero Service, and a nil *Service, are "not configured".
type Service struct {
	sender  Sender
	info    Info
	catalog *Catalog
}

// NewService returns a Service that sends through sender. info is what admins are told.
func NewService(sender Sender, info Info) *Service {
	return &Service{sender: sender, info: info, catalog: DefaultCatalog()}
}

// Configured reports whether the server can send email.
func (s *Service) Configured() bool { return s != nil && s.sender != nil }

// Info describes the configured server; it is the zero Info when there is none.
func (s *Service) Info() Info {
	if !s.Configured() {
		return Info{}
	}
	return s.info
}

// Catalog is the translated templates.
func (s *Service) Catalog() *Catalog {
	if s == nil || s.catalog == nil {
		return DefaultCatalog()
	}
	return s.catalog
}

// Send sends m, or returns ErrNotConfigured.
func (s *Service) Send(ctx context.Context, m Message) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	return s.sender.Send(ctx, m)
}

// SendTemplate renders the catalog's message name in the recipient's language, locale (a
// BCP 47 tag, or "" for none), and sends it to to. params fills the template's
// placeholders.
func (s *Service) SendTemplate(ctx context.Context, to, locale, name string, params map[string]string) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	c, err := s.Catalog().Render(locale, name, params)
	if err != nil {
		return err
	}
	return s.sender.Send(ctx, Message{To: []string{to}, Subject: c.Subject, Text: c.Text, HTML: c.HTML})
}
