package notifications

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"strings"
	"time"

	"taskto.ai/taskto/internal/serverkey"
)

const (
	recordSize   = 4096
	keyIDSize    = 65
	headerSize   = 16 + 4 + 1 + keyIDSize
	maxPlaintext = recordSize - headerSize - 1 - 16
	authSize     = 16
)

var b64 = base64.RawURLEncoding

func encrypt(uaPublic, authSecret, plaintext []byte) ([]byte, error) {
	asPrivate, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	return encryptWith(asPrivate, salt, uaPublic, authSecret, plaintext)
}

func encryptWith(asPrivate *ecdh.PrivateKey, salt, uaPublic, authSecret, plaintext []byte) ([]byte, error) {
	if len(plaintext) > maxPlaintext {
		return nil, errors.New("notifications: the message is too long for one record")
	}
	if len(salt) != 16 || len(authSecret) != authSize {
		return nil, errors.New("notifications: bad salt or auth secret")
	}
	uaKey, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("notifications: the browser's key: %w", err)
	}
	shared, err := asPrivate.ECDH(uaKey)
	if err != nil {
		return nil, err
	}
	asPublic := asPrivate.PublicKey().Bytes()

	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, err
	}
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, append(append([]byte{}, plaintext...), 0x02), nil)

	out := make([]byte, 0, headerSize+len(sealed))
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, keyIDSize)
	out = append(out, asPublic...)
	return append(out, sealed...), nil
}

type vapidKey struct {
	private *ecdsa.PrivateKey
	public  []byte
}

var vapidAD = []byte("taskto push_vapid v1")

func loadVAPID(ctx context.Context, db *sql.DB, key *serverkey.Key) (*vapidKey, error) {
	read := func() (*vapidKey, error) {
		var pub, sealed []byte
		err := db.QueryRowContext(ctx, `SELECT public_key, private_sealed FROM push_vapid WHERE id = 1`).Scan(&pub, &sealed)
		if err != nil {
			return nil, err
		}
		raw, err := key.Open(sealed, vapidAD)
		if err != nil {
			return nil, fmt.Errorf("notifications: the VAPID key can't be opened (is server.key the one it was made with?): %w", err)
		}
		priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
		if err != nil {
			return nil, fmt.Errorf("notifications: the VAPID key: %w", err)
		}
		return &vapidKey{private: priv, public: pub}, nil
	}
	k, err := read()
	if !errors.Is(err, sql.ErrNoRows) {
		return k, err
	}
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	sealed, err := key.Seal(priv.Bytes(), vapidAD)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO push_vapid (id, public_key, private_sealed) VALUES (1, ?, ?) ON CONFLICT (id) DO NOTHING`,
		priv.PublicKey().Bytes(), sealed); err != nil {
		return nil, err
	}
	return read()
}

func (k *vapidKey) publicKey() string { return b64.EncodeToString(k.public) }

const tokenLifetime = 12 * time.Hour

func (k *vapidKey) authorization(endpoint *url.URL, subject string, now time.Time) (string, error) {
	header, _ := json.Marshal(map[string]string{"typ": "JWT", "alg": "ES256"})
	claims, err := json.Marshal(map[string]any{
		"aud": origin(endpoint),
		"exp": now.Add(tokenLifetime).Unix(),
		"sub": subject,
	})
	if err != nil {
		return "", err
	}
	signing := b64.EncodeToString(header) + "." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	der, err := ecdsa.SignASN1(rand.Reader, k.private, digest[:])
	if err != nil {
		return "", err
	}
	var sig struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(der, &sig); err != nil {
		return "", err
	}
	raw := make([]byte, 64)
	sig.R.FillBytes(raw[:32])
	sig.S.FillBytes(raw[32:])
	return "vapid t=" + signing + "." + b64.EncodeToString(raw) + ", k=" + k.publicKey(), nil
}

func origin(u *url.URL) string {
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if port == "" || (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return scheme + "://" + host
	}
	return scheme + "://" + host + ":" + port
}
