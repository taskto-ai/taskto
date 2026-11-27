package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
)

func NewSetupToken() string {
	token, _ := newToken()
	return token
}

func SetupNeeded(ctx context.Context, sqlDB *sql.DB) (bool, error) {
	var exists bool
	err := sqlDB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&exists)
	return !exists, err
}

func (s *Service) validSetupToken(token string) bool {
	if s.setupHash == nil || s.setupDone.Load() {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(sum[:], s.setupHash) == 1
}
