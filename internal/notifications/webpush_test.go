package notifications

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crypto/aes"
	"crypto/cipher"

	"taskto.ai/taskto/internal/db"
	"taskto.ai/taskto/internal/serverkey"
)

// Client is a browser's push subscription keys, for tests: the side that decrypts what a push
// carries, written from RFC 8291 section 3.4 and independent of encrypt.
type Client struct {
	Private *ecdh.PrivateKey
	Auth    []byte
}

// NewClient makes a browser's keys.
func NewClient(t testing.TB) *Client {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, authSize)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	return &Client{Private: priv, Auth: secret}
}

// Public is the key as a browser reports it (p256dh), base64url.
func (c *Client) Public() string { return b64.EncodeToString(c.Private.PublicKey().Bytes()) }

// AuthSecret is the auth secret as a browser reports it, base64url.
func (c *Client) AuthSecret() string { return b64.EncodeToString(c.Auth) }

// Decrypt reads the body of a push: the message encrypted to the client, or an error.
func (c *Client) Decrypt(body []byte) ([]byte, error) {
	if len(body) < headerSize+16 {
		return nil, errors.New("too short")
	}
	salt := body[:16]
	rs := binary.BigEndian.Uint32(body[16:20])
	idLen := int(body[20])
	if idLen != 65 || rs < 18 || int(rs) < len(body)-headerSize {
		return nil, errors.New("bad header")
	}
	asPublic := body[21 : 21+idLen]
	ciphertext := body[21+idLen:]
	asKey, err := ecdh.P256().NewPublicKey(asPublic)
	if err != nil {
		return nil, err
	}
	shared, err := c.Private.ECDH(asKey)
	if err != nil {
		return nil, err
	}
	prkKey, _ := hkdf.Extract(sha256.New, shared, c.Auth)
	info := append(append([]byte("WebPush: info\x00"), c.Private.PublicKey().Bytes()...), asPublic...)
	ikm, _ := hkdf.Expand(sha256.New, prkKey, string(info), 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	// The last record ends in 0x02 after any zero padding.
	end := len(plain)
	for end > 0 && plain[end-1] == 0 {
		end--
	}
	if end == 0 || plain[end-1] != 0x02 {
		return nil, errors.New("no delimiter")
	}
	return plain[:end-1], nil
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	out, err := b64.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// RFC 8291 appendix A: the standard's own example, byte for byte.
func TestEncryptionMatchesRFC8291Example(t *testing.T) {
	uaPublic := mustB64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	authSecret := mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg")
	asPrivate, err := ecdh.P256().NewPrivateKey(mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	salt := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw")
	want := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN")

	got, err := encryptWith(asPrivate, salt, uaPublic, authSecret, []byte("When I grow up, I want to be a watermelon"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("encrypted = %s\nwant      %s", b64.EncodeToString(got), b64.EncodeToString(want))
	}
}

func TestEncryptedMessageDecryptsAtTheBrowser(t *testing.T) {
	c := NewClient(t)
	for _, msg := range []string{`{"id":1,"kind":"changed"}`, "", strings.Repeat("x", maxPlaintext)} {
		body, err := encrypt(c.Private.PublicKey().Bytes(), c.Auth, []byte(msg))
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > recordSize+16 { // a record, and nothing past the standard's single one
			t.Fatalf("a %d byte message became %d bytes", len(msg), len(body))
		}
		got, err := c.Decrypt(body)
		if err != nil || string(got) != msg {
			t.Fatalf("decrypted = %q, %v; want %q", got, err, msg)
		}
		// Another browser, with other keys, can't read it, and a tampered message isn't read.
		if _, err := NewClient(t).Decrypt(body); err == nil {
			t.Fatal("another browser read the message")
		}
		body[len(body)-1] ^= 1
		if _, err := c.Decrypt(body); err == nil {
			t.Fatal("a tampered message was read")
		}
	}
	if _, err := encrypt(c.Private.PublicKey().Bytes(), c.Auth, make([]byte, maxPlaintext+1)); err == nil {
		t.Fatal("a message past one record was encrypted")
	}
	if _, err := encrypt([]byte("not a key"), c.Auth, []byte("x")); err == nil {
		t.Fatal("encrypted to a key that isn't one")
	}
}

func openTestDB(t *testing.T) (*serverkey.Key, *vapidKey) {
	t.Helper()
	sqlDB, err := db.Open(t.Context(), filepath.Join(t.TempDir(), db.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := db.Migrate(t.Context(), sqlDB); err != nil {
		t.Fatal(err)
	}
	key, err := serverkey.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := loadVAPID(t.Context(), sqlDB, key)
	if err != nil {
		t.Fatal(err)
	}
	// Made once: a second start reads the same pair back, and a key that isn't the server's
	// can't open it.
	again, err := loadVAPID(t.Context(), sqlDB, key)
	if err != nil || !bytes.Equal(again.public, first.public) || !again.private.Equal(first.private) {
		t.Fatalf("the key pair changed on a second start: %v", err)
	}
	other, _ := serverkey.Load(t.TempDir())
	if _, err := loadVAPID(t.Context(), sqlDB, other); err == nil {
		t.Fatal("the VAPID key opened with another server key")
	}
	var stored []byte
	if err := sqlDB.QueryRow(`SELECT private_sealed FROM push_vapid`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	scalar, err := first.private.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, scalar) {
		t.Fatal("the private key is in the database in clear")
	}
	return key, first
}

func TestVAPIDAuthorizationIsAValidES256Token(t *testing.T) {
	_, k := openTestDB(t)
	endpoint, _ := url.Parse("https://push.example.net:8443/send/abc")
	now := time.Unix(1_700_000_000, 0)
	header, err := k.authorization(endpoint, "https://example.org/contact", now)
	if err != nil {
		t.Fatal(err)
	}
	rest, ok := strings.CutPrefix(header, "vapid t=")
	if !ok {
		t.Fatalf("header = %q", header)
	}
	token, key, ok := strings.Cut(rest, ", k=")
	if !ok || key != k.publicKey() {
		t.Fatalf("header = %q, want the public key %s", header, k.publicKey())
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token = %q", token)
	}
	var head map[string]string
	var claims struct {
		Aud, Sub string
		Exp      int64
	}
	if err := json.Unmarshal(mustB64(t, parts[0]), &head); err != nil || head["alg"] != "ES256" || head["typ"] != "JWT" {
		t.Fatalf("header = %v, %v", head, err)
	}
	if err := json.Unmarshal(mustB64(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != "https://push.example.net:8443" || claims.Sub != "https://example.org/contact" ||
		claims.Exp != now.Add(12*time.Hour).Unix() {
		t.Fatalf("claims = %+v", claims)
	}
	// Verified as a push service would: the raw r||s signature, with the public key in the header.
	sig := mustB64(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want 64", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), mustB64(t, key))
	if err != nil {
		t.Fatal(err)
	}
	der, err := asn1.Marshal(struct{ R, S *big.Int }{new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])})
	if err != nil {
		t.Fatal(err)
	}
	if !ecdsa.VerifyASN1(pub, digest[:], der) {
		t.Fatal("the signature doesn't verify under the public key")
	}
}

// Purge is the retention the worker runs every hour, for the external tests.
func (s *Service) Purge(ctx context.Context) error { return s.purge(ctx) }

// Tidy is what the worker does on its own schedule, once, for the external tests.
func (s *Service) Tidy(ctx context.Context) { s.tidy(ctx) }

// TidyEvery sets how often the worker tidies, for the external tests.
func (s *Service) TidyEvery(d time.Duration) { s.tidyEvery = d }

func TestVAPIDAudienceIsTheSerializedOrigin(t *testing.T) {
	_, k := openTestDB(t)
	for raw, want := range map[string]string{
		"https://push.example.net/send/abc":        "https://push.example.net",
		"HTTPS://Push.Example.NET/send/abc":        "https://push.example.net",
		"https://push.example.net:443/send/abc":    "https://push.example.net",
		"https://PUSH.example.net:8443/send/abc":   "https://push.example.net:8443",
		"https://push.example.net./x":              "https://push.example.net.",
		"https://[2001:DB8::1]:443/x":              "https://[2001:db8::1]",
		"https://[2001:db8::1]:8443/x?a=b#ignored": "https://[2001:db8::1]:8443",
	} {
		endpoint, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		header, err := k.authorization(endpoint, "https://example.org", time.Unix(1_700_000_000, 0))
		if err != nil {
			t.Fatal(err)
		}
		token, _, _ := strings.Cut(strings.TrimPrefix(header, "vapid t="), ", k=")
		var claims struct{ Aud string }
		if err := json.Unmarshal(mustB64(t, strings.Split(token, ".")[1]), &claims); err != nil {
			t.Fatal(err)
		}
		if claims.Aud != want {
			t.Errorf("aud for %s = %q, want %q", raw, claims.Aud, want)
		}
	}
}
