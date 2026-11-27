package notifications

import (
	"context"
	"database/sql"

	"taskto.ai/taskto/internal/mail"
	"taskto.ai/taskto/internal/schedule"
)

func (s *Service) AfterReserve(f func()) { s.afterReserve = f }

func (s *Service) BetweenReads(f func(*sql.Tx)) { s.betweenReads = f }

func (s *Service) SetMail(m *mail.Service) { s.mail = m }

const MaxInstantPerHour = maxInstantPerHour

func (s *Service) DigestNext(ctx context.Context, job schedule.Job) error {
	return s.digestNext(ctx, job)
}
