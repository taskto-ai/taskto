package auth

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// providersFile writes a providers file and a client secret file next to it, and returns the
// file's path; %SECRET% in json stands for the secret file's path.
func providersFile(t *testing.T, json string) string {
	t.Helper()
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("  fake-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sso.json")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(json, "%SECRET%", secret)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadSSOProviders(t *testing.T) {
	path := providersFile(t, `{"providers": [
		{"id": "google", "name": "Google", "issuer": "https://accounts.example.com",
		 "client_id": "cid", "client_secret": {"file": "%SECRET%"}},
		{"id": "authentik-2", "name": " Authentik ", "issuer": "https://auth.example.com/application/o/taskto/",
		 "client_id": "cid2", "client_secret": {"env": "FAKE_SSO_SECRET"},
		 "scopes": ["openid", "email"], "trust_email": true}
	]}`)
	got, err := LoadSSOProviders(path, func(k string) string {
		if k == "FAKE_SSO_SECRET" {
			return " from-env "
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []SSOProvider{
		{ID: "google", Name: "Google", Issuer: "https://accounts.example.com", ClientID: "cid",
			ClientSecret: "fake-secret-value", Scopes: []string{"openid", "email", "profile"}},
		{ID: "authentik-2", Name: "Authentik", Issuer: "https://auth.example.com/application/o/taskto/", ClientID: "cid2",
			ClientSecret: "from-env", Scopes: []string{"openid", "email"}, TrustEmail: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLoadSSOProvidersRefusals(t *testing.T) {
	entry := func(change func(string) string) string {
		base := `{"id": "p", "name": "P", "issuer": "https://idp.example.com", "client_id": "c", "client_secret": {"file": "%SECRET%"}}`
		return `{"providers": [` + change(base) + `]}`
	}
	swap := func(old, new string) func(string) string {
		return func(s string) string {
			if !strings.Contains(s, old) {
				panic("no " + old)
			}
			return strings.Replace(s, old, new, 1)
		}
	}
	const one = `{"id": "p", "name": "P", "issuer": "https://idp.example.com", "client_id": "c", "client_secret": {"file": "%SECRET%"}}`
	for name, tt := range map[string]struct{ json, want string }{
		"not json":               {`{"providers": [`, "not valid JSON"},
		"trailing data":          {`{"providers": [{}]} {}`, "one JSON object"},
		"no providers":           {`{"providers": []}`, "providers is empty"},
		"an unknown field":       {entry(swap(`"id": "p"`, `"id": "p", "colour": "red"`)), `unknown field "colour"`},
		"an unknown top field":   {`{"providers": [], "extra": 1}`, `unknown field "extra"`},
		"an unknown secret key":  {entry(swap(`{"file":`, `{"path": "x", "file":`)), `unknown field "path"`},
		"a wrong type":           {entry(swap(`"client_id": "c"`, `"client_id": 5`)), "client_id"},
		"an id with capitals":    {entry(swap(`"id": "p"`, `"id": "Google"`)), "providers[0].id"},
		"an id too long":         {entry(swap(`"id": "p"`, `"id": "`+strings.Repeat("a", 33)+`"`)), "providers[0].id"},
		"an id with a slash":     {entry(swap(`"id": "p"`, `"id": "a/b"`)), "providers[0].id"},
		"no name":                {entry(swap(`"name": "P"`, `"name": " "`)), "providers[0].name"},
		"an http issuer":         {entry(swap("https://idp", "http://idp")), "providers[0].issuer"},
		"an issuer with a query": {entry(swap("idp.example.com", "idp.example.com?a=b")), "providers[0].issuer"},
		"no client id":           {entry(swap(`"client_id": "c"`, `"client_id": ""`)), "providers[0].client_id"},
		"no secret":              {entry(swap(`, "client_secret": {"file": "%SECRET%"}`, "")), "providers[0].client_secret"},
		"two secret sources":     {entry(swap(`{"file": "%SECRET%"}`, `{"file": "%SECRET%", "env": "X"}`)), "providers[0].client_secret"},
		"a relative secret":      {entry(swap(`"%SECRET%"`, `"secret"`)), "client_secret.file: want an absolute path"},
		"a missing secret file":  {entry(swap(`"%SECRET%"`, `"/nonexistent/secret"`)), "client_secret.file"},
		"an empty variable":      {entry(swap(`{"file": "%SECRET%"}`, `{"env": "FAKE_EMPTY"}`)), "the file or variable is empty"},
		"scopes without openid":  {entry(swap(`"client_id"`, `"scopes": ["email"], "client_id"`)), `must include "openid"`},
		"a scope with a space":   {entry(swap(`"client_id"`, `"scopes": ["openid", "a b"], "client_id"`)), "providers[0].scopes"},
		"the same id twice":      {`{"providers": [` + one + `,` + one + `]}`, "providers[1].id: used twice"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadSSOProviders(providersFile(t, tt.json), func(string) string { return "" })
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one with %q", err, tt.want)
			}
			// The error names fields, never values: no secret, client id or issuer in it.
			for _, leak := range []string{"fake-secret-value", `"c"`, "idp.example.com"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("the error quotes %q: %v", leak, err)
				}
			}
		})
	}
	if _, err := LoadSSOProviders(filepath.Join(t.TempDir(), "missing.json"), os.Getenv); err == nil {
		t.Fatal("a missing file loaded")
	}
}

func TestIssuerMayBeHTTPOnlyOnLoopback(t *testing.T) {
	for issuer, want := range map[string]bool{
		"https://idp.example.com":     true,
		"https://idp.example.com/a/":  true,
		"http://localhost:9000":       true,
		"http://127.0.0.1:9000":       true,
		"http://[::1]:9000":           true,
		"http://idp.example.com":      false,
		"ftp://idp.example.com":       false,
		"https://":                    false,
		"https://u:p@idp.example.com": false,
		"https://idp.example.com#f":   false,
	} {
		if got := validIssuer(issuer); got != want {
			t.Errorf("validIssuer(%q) = %v, want %v", issuer, got, want)
		}
	}
}
