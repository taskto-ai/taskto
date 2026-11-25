package notifications

import (
	"context"
	"database/sql"

	"taskto.ai/taskto/internal/mail"
	"taskto.ai/taskto/internal/schedule"
)

// AfterReserve sets what runs between a mail's reservation and its last check, for the external tests.
func (s *Service) AfterReserve(f func()) { s.afterReserve = f }

// BetweenReads sets what runs in the final read of an instant mail, between the access and the address,
// with its transaction, for the external tests.
func (s *Service) BetweenReads(f func(*sql.Tx)) { s.betweenReads = f }

// SetMail replaces the service's mail, for the external tests: nil is a server with none.
func (s *Service) SetMail(m *mail.Service) { s.mail = m }

// MaxInstantPerHour is the most instant mails a person is sent in an hour.
const MaxInstantPerHour = maxInstantPerHour

// DigestNext schedules the person's next summary after job, for the external tests.
func (s *Service) DigestNext(ctx context.Context, job schedule.Job) error {
	return s.digestNext(ctx, job)
}
