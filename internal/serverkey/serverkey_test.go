package serverkey

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeValue = "fake-api-key-for-tests-not-real"

func load(t *testing.T, dir string) *Key {
	t.Helper()
	k, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestFirstRunCreatesAnOwnerOnlyKey(t *testing.T) {
	dir := t.TempDir()
	k := load(t, dir)
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != keySize {
		t.Fatalf("key file: mode %o, %d bytes; want 600 and %d", info.Mode().Perm(), info.Size(), keySize)
	}

	sealed, err := k.Seal([]byte(fakeValue), []byte("row 1"))
	if err != nil {
		t.Fatal(err)
	}
	again := load(t, dir)
	if plain, err := again.Open(sealed, []byte("row 1")); err != nil || string(plain) != fakeValue {
		t.Fatalf("open after reload = %q, %v", plain, err)
	}
}

func TestRoundTrip(t *testing.T) {
	k := load(t, t.TempDir())
	for _, plain := range []string{fakeValue, "", strings.Repeat("x", 5000)} {
		sealed, err := k.Seal([]byte(plain), []byte("ad"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(sealed, []byte(fakeValue)) {
			t.Fatal("the sealed value contains the plaintext")
		}
		got, err := k.Open(sealed, []byte("ad"))
		if err != nil || string(got) != plain {
			t.Fatalf("round trip of %d bytes = %d bytes, %v", len(plain), len(got), err)
		}
	}
	a, _ := k.Seal([]byte(fakeValue), nil)
	b, _ := k.Seal([]byte(fakeValue), nil)
	if bytes.Equal(a, b) {
		t.Fatal("two seals of the same value are identical")
	}
}

func TestTamperingIsDetected(t *testing.T) {
	k := load(t, t.TempDir())
	ad := []byte("taskto provider_accounts 1 api_key")
	sealed, err := k.Seal([]byte(fakeValue), ad)
	if err != nil {
		t.Fatal(err)
	}
	for i := range sealed {
		for bit := range 8 {
			bad := bytes.Clone(sealed)
			bad[i] ^= 1 << bit
			if _, err := k.Open(bad, ad); !errors.Is(err, ErrOpen) {
				t.Fatalf("byte %d bit %d flipped: err = %v, want ErrOpen", i, bit, err)
			}
		}
	}
	cases := map[string]struct {
		sealed, ad []byte
		key        *Key
	}{
		"other associated data": {sealed, []byte("taskto provider_accounts 2 api_key"), k},
		"no associated data":    {sealed, nil, k},
		"truncated":             {sealed[:len(sealed)-1], ad, k},
		"extended":              {append(bytes.Clone(sealed), 0), ad, k},
		"empty":                 {nil, ad, k},
		"just a header":         {sealed[:13], ad, k},
		"another server's key":  {sealed, ad, load(t, t.TempDir())},
	}
	for name, tc := range cases {
		if _, err := tc.key.Open(tc.sealed, tc.ad); !errors.Is(err, ErrOpen) {
			t.Errorf("%s: err = %v, want ErrOpen", name, err)
		}
	}
}

func TestBadKeyFilesAreRefused(t *testing.T) {
	write := func(t *testing.T, content []byte, mode os.FileMode) string {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, FileName)
		if err := os.WriteFile(path, content, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	key := bytes.Repeat([]byte{7}, keySize)
	cases := map[string]struct {
		dir  func(t *testing.T) string
		want string
	}{
		"readable by others": {func(t *testing.T) string { return write(t, key, 0o644) }, "want 600"},
		"too short":          {func(t *testing.T) string { return write(t, key[:16], 0o600) }, "want 32"},
		"too long":           {func(t *testing.T) string { return write(t, append(key, 1), 0o600) }, "want 32"},
		"a directory": {func(t *testing.T) string {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, FileName), 0o700); err != nil {
				t.Fatal(err)
			}
			return dir
		}, "not a regular file"},
		"a symlink": {func(t *testing.T) string {
			target := filepath.Join(write(t, key, 0o600), FileName)
			dir := t.TempDir()
			if err := os.Symlink(target, filepath.Join(dir, FileName)); err != nil {
				t.Fatal(err)
			}
			return dir
		}, "serverkey"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := tc.dir(t)
			_, err := Load(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load = %v, want an error containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "\x07") {
				t.Fatalf("the error quotes the key file: %q", err)
			}
		})
	}
}

func TestMAC(t *testing.T) {
	dir := t.TempDir()
	k := load(t, dir)
	mac := k.MAC("purpose", []byte("7"))
	if len(mac) != 32 {
		t.Fatalf("MAC is %d bytes, want 32", len(mac))
	}
	if again := load(t, dir).MAC("purpose", []byte("7")); !bytes.Equal(mac, again) {
		t.Fatal("the MAC changed after reloading the key")
	}
	for name, other := range map[string][]byte{
		"another message":    k.MAC("purpose", []byte("8")),
		"another purpose":    k.MAC("purpose2", []byte("7")),
		"a shifted boundary": k.MAC("purpose7", nil),
		"another key":        load(t, t.TempDir()).MAC("purpose", []byte("7")),
	} {
		if bytes.Equal(mac, other) {
			t.Errorf("%s gives the same MAC", name)
		}
	}
}
