package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"taskto.ai/taskto/internal/apierr"
)

// Params are argon2id's cost settings. A hash records the ones it was made with, so
// changing them only affects new hashes.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32 // passes over the memory
	Threads uint8
}

// DefaultParams are the second recommended option of RFC 9106 (section 4): 64 MiB, three
// passes and four lanes. A hash takes some tens of milliseconds on a current CPU.
var DefaultParams = Params{Memory: 64 * 1024, Time: 3, Threads: 4}

const (
	saltLen = 16
	keyLen  = 32

	// MinPasswordLen and MaxPasswordLen bound a new password, in characters and bytes.
	// The upper bound only stops a client from sending megabytes to hash.
	MinPasswordLen = 12
	MaxPasswordLen = 1024

	// maxHashing is how many hashes run at once. Each holds Params.Memory, so this caps
	// the memory a burst of logins can take.
	maxHashing = 2
)

// errBadHash means a stored hash couldn't be parsed.
var errBadHash = errors.New("auth: malformed password hash")

// hasher hashes and checks passwords, at most maxHashing at a time.
type hasher struct {
	params Params
	slots  chan struct{}

	dummyOnce sync.Once
	dummy     string
}

func newHasher(p Params) *hasher {
	if p == (Params{}) {
		p = DefaultParams
	}
	return &hasher{params: p, slots: make(chan struct{}, maxHashing)}
}

// acquire waits for a free hashing slot, or until ctx is done.
func (h *hasher) acquire(ctx context.Context) error {
	select {
	case h.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *hasher) release() { <-h.slots }

// hash returns password's argon2id hash in the PHC string format:
// $argon2id$v=19$m=65536,t=3,p=4$<salt>$<key>, with unpadded standard base64.
func (h *hasher) hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	p := h.params
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// verify reports whether password matches encoded, comparing in constant time.
func (h *hasher) verify(ctx context.Context, password, encoded string) (bool, error) {
	p, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(key))) // #nosec G115 -- decodeHash bounds len(key).
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

// burn does the work of a failed verify, for a login with an unknown email, so the reply
// takes as long as one for a known email with a wrong password.
func (h *hasher) burn(ctx context.Context, password string) {
	h.dummyOnce.Do(func() {
		// Not ctx: a cancelled first caller mustn't leave the dummy empty for good.
		h.dummy, _ = h.hash(context.Background(), "not a password: equalises the time of a failed login")
	})
	if h.dummy != "" {
		_, _ = h.verify(ctx, password, h.dummy)
	}
}

func decodeHash(encoded string) (Params, []byte, []byte, error) {
	// "", "argon2id", "v=19", "m=…,t=…,p=…", salt, key
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Params{}, nil, nil, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return Params{}, nil, nil, errBadHash
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil ||
		p.Memory == 0 || p.Time == 0 || p.Threads == 0 {
		return Params{}, nil, nil, errBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return Params{}, nil, nil, errBadHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > 128 {
		return Params{}, nil, nil, errBadHash
	}
	return p, salt, key, nil
}

// checkPassword says what is wrong with a new password, or returns nil.
func checkPassword(password string) *apierr.Error {
	switch {
	case utf8.RuneCountInString(password) < MinPasswordLen:
		return apierr.New(http.StatusBadRequest, apierr.CodePasswordTooShort,
			fmt.Sprintf("the password must have at least %d characters", MinPasswordLen), apierr.Params{"min": MinPasswordLen})
	case len(password) > MaxPasswordLen:
		return apierr.New(http.StatusBadRequest, apierr.CodePasswordTooLong,
			fmt.Sprintf("the password must have at most %d bytes", MaxPasswordLen), apierr.Params{"max": MaxPasswordLen})
	}
	return nil
}
