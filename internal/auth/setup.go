package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
)

// First-run setup is guarded by a setup token, so whoever reaches a fresh server first
// can't claim it: the server makes one at startup while no user exists and prints it to
// its log (see cmd/taskto), and POST /api/setup requires it. Only someone who can read
// the server's log, that is its operator, can set it up.

// NewSetupToken returns a random 256-bit setup token, URL-safe base64 (43 characters).
func NewSetupToken() string {
	token, _ := newToken()
	return token
}

// SetupNeeded reports whether sqlDB, which must be migrated, has no user yet, so first-run
// setup is still to be done.
func SetupNeeded(ctx context.Context, sqlDB *sql.DB) (bool, error) {
	var exists bool
	err := sqlDB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&exists)
	return !exists, err
}

// validSetupToken reports whether token is this server's setup token. It is false for
// every token once setup is done, and when the server has no setup token.
func (s *Service) validSetupToken(token string) bool {
	if s.setupHash == nil || s.setupDone.Load() {
		return false
	}
	// Both sides are hashed, so the comparison takes the same time whatever the length
	// of what was sent.
	sum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(sum[:], s.setupHash) == 1
}
