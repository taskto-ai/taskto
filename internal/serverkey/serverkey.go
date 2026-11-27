// Package serverkey holds the server's own encryption key and seals secrets with it, such as
// the API keys of model providers, so the database never holds them in clear.
//
// The key is 32 random bytes in <data>/server.key, created with mode 0600 on first run. It is
// never logged and never leaves this package. Losing it makes every sealed secret unreadable,
// and anyone holding it and a copy of the database can read them: back it up, but apart from
// the database.
//
// Sealing is AES-256-GCM from the standard library with a random 96-bit nonce. A sealed value
// is version (1 byte) || nonce (12 bytes) || ciphertext and tag. Callers pass associated data
// naming where the value belongs (such as a table row), so a sealed value copied to another
// place doesn't open.
//
// MAC authenticates values the server hands out and later checks, such as the agent's
// per-conversation capabilities, with HMAC-SHA256 under a key derived from the server key
// (never the sealing key itself), separated by purpose.
package serverkey

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// FileName is the key's file name inside the data directory.
const FileName = "server.key"

const (
	keySize = 32 // AES-256
	version = 1
)

// ErrOpen is returned by Open for a value that was tampered with, sealed with another key or
// for other associated data, or isn't a sealed value at all.
var ErrOpen = errors.New("serverkey: cannot open the sealed value")

// Key seals and opens secrets. It is safe for concurrent use.
type Key struct {
	aead cipher.AEAD
	mac  []byte // HMAC key derived from the server key, for MAC
}

// Load reads the key in dataDir, creating it on first run. An existing key file must be a
// regular file of 32 bytes that only its owner can read or write.
func Load(dataDir string) (*Key, error) {
	path := filepath.Join(dataDir, FileName)
	raw, err := read(path)
	if errors.Is(err, os.ErrNotExist) {
		raw, err = create(path)
	}
	if err != nil {
		return nil, err
	}
	return newKey(raw)
}

func newKey(raw []byte) (*Key, error) {
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Key{aead: aead, mac: hmacSum(raw, []byte("taskto serverkey mac v1"))}, nil
}

func hmacSum(key []byte, parts ...[]byte) []byte {
	h := hmac.New(sha256.New, key)
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// MAC returns a 32-byte HMAC-SHA256 of msg for purpose, which names what the value is
// for, so a MAC made for one purpose never checks for another. Compare MACs with
// hmac.Equal.
func (k *Key) MAC(purpose string, msg []byte) []byte {
	return hmacSum(k.mac, []byte(purpose), []byte{0}, msg)
}

// read opens the key file without following a symlink and checks it before reading.
func read(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|noFollow, 0) //nolint:gosec // G304: the path is the data directory's key file
	if err != nil {
		return nil, fmt.Errorf("serverkey: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("serverkey: %w", err)
	}
	switch {
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("serverkey: %s is not a regular file", path)
	case ownerOnly && info.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("serverkey: %s has mode %o; want 600 (chmod 600 %s)", path, info.Mode().Perm(), path)
	case info.Size() != keySize:
		return nil, fmt.Errorf("serverkey: %s is %d bytes; want %d", path, info.Size(), keySize)
	}
	raw := make([]byte, keySize)
	if _, err := io.ReadFull(f, raw); err != nil {
		return nil, fmt.Errorf("serverkey: %w", err)
	}
	return raw, nil
}

// create writes a new random key. O_EXCL: if two processes race on first run, one fails
// instead of both writing a key and one of them using a key that's then replaced.
func create(path string) ([]byte, error) {
	raw := make([]byte, keySize)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0o600) //nolint:gosec // G304: as in read
	if err != nil {
		return nil, fmt.Errorf("serverkey: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("serverkey: %w", err)
	}
	// Synced before use: a key lost in a crash would make the secrets sealed with it unreadable.
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("serverkey: %w", err)
	}
	return raw, nil
}

// Seal encrypts plaintext, bound to the associated data ad.
func (k *Key) Seal(plaintext, ad []byte) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	// No size worked out ahead: Seal appends what it needs.
	out := append([]byte{version}, nonce...)
	return k.aead.Seal(out, nonce, plaintext, ad), nil
}

// Open decrypts a value from Seal with the same associated data. Any change to the value, or
// different associated data, is ErrOpen.
func (k *Key) Open(sealed, ad []byte) ([]byte, error) {
	n := k.aead.NonceSize()
	if len(sealed) < 1+n+k.aead.Overhead() || sealed[0] != version {
		return nil, ErrOpen
	}
	plain, err := k.aead.Open(nil, sealed[1:1+n], sealed[1+n:], ad)
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}
