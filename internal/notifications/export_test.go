package notifications

import "taskto.ai/taskto/internal/mail"

// AfterReserve sets what runs between a mail's reservation and its last check, for the external tests.
func (s *Service) AfterReserve(f func()) { s.afterReserve = f }

// SetMail replaces the service's mail, for the external tests: nil is a server with none.
func (s *Service) SetMail(m *mail.Service) { s.mail = m }

// MaxInstantPerHour is the most instant mails a person is sent in an hour.
const MaxInstantPerHour = maxInstantPerHour
