package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// newToken returns a random 256-bit token, URL-safe base64 without padding (43
// characters), and the hash to store for it.
func newToken() (token, hash string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token)
}

// hashToken is what the database keeps for a session or invite token. A token has 256
// random bits, so a plain SHA-256 is enough: there is nothing to brute-force, and a
// leaked database doesn't give working tokens.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
