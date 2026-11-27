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

const FileName = "server.key"

const (
	keySize = 32
	version = 1
)

var ErrOpen = errors.New("serverkey: cannot open the sealed value")

type Key struct {
	aead cipher.AEAD
	mac  []byte
}

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

func (k *Key) MAC(purpose string, msg []byte) []byte {
	return hmacSum(k.mac, []byte(purpose), []byte{0}, msg)
}

func read(path string) ([]byte, error) {
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDONLY|noFollow, 0)
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

func create(path string) ([]byte, error) {
	raw := make([]byte, keySize)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0o600)
	if err != nil {
		return nil, fmt.Errorf("serverkey: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("serverkey: %w", err)
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("serverkey: %w", err)
	}
	return raw, nil
}

func (k *Key) Seal(plaintext, ad []byte) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{version}, nonce...)
	return k.aead.Seal(out, nonce, plaintext, ad), nil
}

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
