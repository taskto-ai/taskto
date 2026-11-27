package mail

import (
	"context"
	"errors"
	"fmt"
)

type Message struct {
	To      []string
	Subject string
	Text    string
	HTML    string
}

type Sender interface {
	Send(ctx context.Context, m Message) error
}

var ErrNotConfigured = errors.New("mail: outgoing email isn't configured")

type Stage string

const (
	StageConnect  Stage = "connect"
	StageTLS      Stage = "tls"
	StageAuth     Stage = "auth"
	StageRejected Stage = "rejected"
	StageSend     Stage = "send"
)

type SendError struct {
	Stage     Stage
	Code      int
	Err       error
	Ambiguous bool
}

func (e *SendError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("mail: %s: %d: %v", e.Stage, e.Code, e.Err)
	}
	return fmt.Sprintf("mail: %s: %v", e.Stage, e.Err)
}

func (e *SendError) Unwrap() error { return e.Err }

type Info struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"`
	From     string `json:"from"`
	SignsIn  bool   `json:"-"`
}

type Service struct {
	sender  Sender
	info    Info
	catalog *Catalog
}

func NewService(sender Sender, info Info) *Service {
	return &Service{sender: sender, info: info, catalog: DefaultCatalog()}
}

func (s *Service) Configured() bool { return s != nil && s.sender != nil }

func (s *Service) Info() Info {
	if !s.Configured() {
		return Info{}
	}
	return s.info
}

func (s *Service) Catalog() *Catalog {
	if s == nil || s.catalog == nil {
		return DefaultCatalog()
	}
	return s.catalog
}

func (s *Service) Send(ctx context.Context, m Message) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	return s.sender.Send(ctx, m)
}

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
