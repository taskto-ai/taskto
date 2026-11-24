package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- HMAC-SHA1 is what RFC 6238 authenticator apps implement
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
)

// Time-based one-time codes (RFC 6238, with RFC 4226's truncation): six digits of an
// HMAC-SHA1 of the 30-second step the time falls in, which is what authenticator apps make.
const (
	totpDigits = 6
	totpPeriod = 30 // seconds
	// totpSkew is how many steps either side of the current one are accepted, for clocks that
	// differ a little.
	totpSkew = 1
	// totpSecretBytes is the secret's length: 160 bits, as RFC 4226 recommends.
	totpSecretBytes = 20
	// totpIssuer is the name authenticator apps list the account under.
	totpIssuer = "TaskTo"
)

var secretEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func newTOTPSecret() []byte {
	b := make([]byte, totpSecretBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return b
}

// totpCode is the code for the given step.
func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step)) // #nosec G115 -- steps are positive
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range totpDigits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, bin%mod)
}

// totpStep is the step a time falls in.
func totpStep(unix int64) int64 { return unix / totpPeriod }

// matchTOTP returns the step whose code is code, among the current step and totpSkew
// either side, that is later than lastStep (the last one accepted), or false. A code
// is compared in constant time against every candidate.
func matchTOTP(secret []byte, code string, unix, lastStep int64) (int64, bool) {
	now := totpStep(unix)
	var found int64
	var ok bool
	for step := now - totpSkew; step <= now+totpSkew; step++ {
		match := subtle.ConstantTimeCompare([]byte(code), []byte(totpCode(secret, step))) == 1
		if match && step > lastStep && (!ok || step > found) {
			found, ok = step, true
		}
	}
	return found, ok
}

// totpURI is the otpauth URI authenticator apps read, usually from a QR code.
func totpURI(secret []byte, account string) string {
	label := url.PathEscape(totpIssuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secretEncoding.EncodeToString(secret))
	q.Set("issuer", totpIssuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(totpPeriod))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// Recovery codes: 60 random bits as twelve characters of Crockford's base32 (no i, l, o or
// u), written in three groups of four. Typing is forgiving: case, spaces and dashes don't
// matter, and o, i and l read as 0, 1 and 1.
const (
	recoveryCodeCount = 10
	recoveryAlphabet  = "0123456789abcdefghjkmnpqrstvwxyz"
	recoveryLen       = 12
)

func newRecoveryCode() string {
	b := make([]byte, recoveryLen)
	_, _ = rand.Read(b)
	var sb strings.Builder
	for i, v := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(recoveryAlphabet[int(v)&31])
	}
	return sb.String()
}

// normalizeCode strips what people type around a code and folds the look-alikes, so a
// TOTP code and a recovery code can be told apart by length and compared as stored.
func normalizeCode(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		switch r {
		case ' ', '-':
		case 'o':
			sb.WriteByte('0')
		case 'i', 'l':
			sb.WriteByte('1')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// isTOTPCode reports whether a normalized code has the shape of a TOTP code.
func isTOTPCode(code string) bool {
	if len(code) != totpDigits {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
