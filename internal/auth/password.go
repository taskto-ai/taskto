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

type Params struct {
	Memory  uint32
	Time    uint32
	Threads uint8
}

var DefaultParams = Params{Memory: 64 * 1024, Time: 3, Threads: 4}

const (
	saltLen         = 16
	keyLen          = 32
	minStoredKeyLen = 16
	maxStoredKeyLen = 128

	MinPasswordLen = 12
	MaxPasswordLen = 1024

	maxHashing = 2
)

var errBadHash = errors.New("auth: malformed password hash")

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

func (h *hasher) acquire(ctx context.Context) error {
	select {
	case h.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *hasher) release() { <-h.slots }

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

func (h *hasher) verify(ctx context.Context, password, encoded string) (bool, error) {
	p, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	size := len(key)
	if size < minStoredKeyLen || size > maxStoredKeyLen {
		return false, errBadHash
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(size))
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

func (h *hasher) burn(ctx context.Context, password string) {
	h.dummyOnce.Do(func() {
		h.dummy, _ = h.hash(context.Background(), "not a password: equalises the time of a failed login")
	})
	if h.dummy != "" {
		_, _ = h.verify(ctx, password, h.dummy)
	}
}

func decodeHash(encoded string) (Params, []byte, []byte, error) {
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
	if err != nil {
		return Params{}, nil, nil, errBadHash
	}
	return p, salt, key, nil
}

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
