package auth

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestHashAndVerify(t *testing.T) {
	ctx := context.Background()
	h := newHasher(cheap)
	encoded, err := h.hash(ctx, "fake-password-one")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("hash = %q, want the PHC format with its parameters", encoded)
	}
	if again, _ := h.hash(ctx, "fake-password-one"); again == encoded {
		t.Fatal("two hashes of one password are equal: the salt isn't random")
	}
	for password, want := range map[string]bool{"fake-password-one": true, "fake-password-two": false, "": false} {
		if ok, err := h.verify(ctx, password, encoded); err != nil || ok != want {
			t.Errorf("verify(%q) = %v, %v; want %v", password, ok, err, want)
		}
	}
	if ok, err := newHasher(Params{Memory: 128, Time: 2, Threads: 2}).verify(ctx, "fake-password-one", encoded); err != nil || !ok {
		t.Fatalf("verify with other default params = %v, %v", ok, err)
	}
}

func TestDefaultParams(t *testing.T) {
	if p := newHasher(Params{}).params; p != DefaultParams || p.Memory < 19*1024 || p.Time < 2 {
		t.Fatalf("default params = %+v, want at least OWASP's argon2id minimum", p)
	}
}

func TestVerifyMalformedHash(t *testing.T) {
	h := newHasher(cheap)
	for _, bad := range []string{
		"",
		"plain",
		"$argon2i$v=19$m=64,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=18$m=64,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=0,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=64,t=1,p=1$!!$a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=64,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5",
	} {
		if ok, err := h.verify(context.Background(), "x", bad); ok || err == nil {
			t.Errorf("verify against %q = %v, %v; want an error", bad, ok, err)
		}
	}
}

func TestHashingWaitsForASlot(t *testing.T) {
	h := newHasher(cheap)
	for range maxHashing {
		h.slots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := h.hash(ctx, "fake-password-one"); err == nil {
		t.Fatal("hash ran with every slot taken")
	}
}

func TestCheckPassword(t *testing.T) {
	for password, ok := range map[string]bool{
		"":                            false,
		"elevenchars":                 false,
		"twelve chars":                true,
		"ünïcödé-pässwörd":            true,
		strings.Repeat("a", 1024):     true,
		strings.Repeat("a", 1025):     false,
		strings.Repeat("é", 12):       true,
		strings.Repeat("é", 11) + "x": true,
	} {
		if got := checkPassword(password) == nil; got != ok {
			t.Errorf("checkPassword(%.20q) accepted = %v, want %v", password, got, ok)
		}
	}
}
