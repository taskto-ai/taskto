package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
)

const (
	totpDigits      = 6
	totpPeriod      = 30
	totpSkew        = 1
	totpSecretBytes = 20
	totpIssuer      = "taskto"
)

var secretEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func newTOTPSecret() []byte {
	b := make([]byte, totpSecretBytes)
	_, _ = rand.Read(b)
	return b
}

func totpCode(secret []byte, step int64) string {
	msg, _ := binary.Append(make([]byte, 0, 8), binary.BigEndian, step)
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg)
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range totpDigits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, bin%mod)
}

func totpStep(unix int64) int64 { return unix / totpPeriod }

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
