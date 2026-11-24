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

// newSessionPublicID names a session to the API and the web app: 128 random bits in hex, never
// reused, unlike the row's integer ID. It is no way into the session, which only its token is.
func newSessionPublicID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// validSessionPublicID reports whether s has the shape of newSessionPublicID's answers.
func validSessionPublicID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
