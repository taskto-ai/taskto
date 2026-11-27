package auth

import (
	"net/http"
	"strings"
	"testing"
)

func TestSetupToken(t *testing.T) {
	e := newEnv(t, Options{})
	body := func(token *string) map[string]string {
		m := map[string]string{"email": "owner@example.com", "name": "Owner", "password": ownerPassword, "workspace_name": "Home"}
		if token != nil {
			m["setup_token"] = *token
		}
		return m
	}
	wrong := NewSetupToken()
	prefix := testSetupToken[:20]
	longer := testSetupToken + "x"
	empty := ""
	for name, token := range map[string]*string{
		"missing":      nil,
		"empty":        &empty,
		"wrong":        &wrong,
		"a prefix":     &prefix,
		"with a tail":  &longer,
		"another case": new(strings.ToUpper(testSetupToken)),
	} {
		t.Run(name, func(t *testing.T) {
			c := e.client()
			rec := c.do(http.MethodPost, "/api/setup", body(token))
			expect(t, rec, http.StatusForbidden)
			if c.cookie != nil {
				t.Fatal("a refused setup set a session cookie")
			}
		})
	}
	if n := e.count(`SELECT count(*) FROM users`); n != 0 {
		t.Fatalf("users = %d after setups without the right token", n)
	}

	owner := e.client()
	expect(t, owner.do(http.MethodPost, "/api/setup", body(&testSetupToken)), http.StatusCreated)

	expect(t, e.client().do(http.MethodPost, "/api/setup", body(&testSetupToken)), http.StatusConflict)
	if e.svc.validSetupToken(testSetupToken) {
		t.Fatal("the setup token is still valid after setup")
	}
	if n := e.count(`SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("users = %d, want 1", n)
	}
}

func TestSetupWithoutToken(t *testing.T) {
	e := newEnv(t, Options{})
	e.svc = New(e.db, Options{HashParams: cheap})
	mux := http.NewServeMux()
	e.svc.Routes(mux)
	e.h = CSRF(mux)
	for _, token := range []string{"", testSetupToken} {
		expect(t, e.client().do(http.MethodPost, "/api/setup", map[string]string{
			"email": "owner@example.com", "name": "Owner", "password": ownerPassword, "workspace_name": "Home",
			"setup_token": token,
		}), http.StatusForbidden)
	}
}

func TestNewSetupToken(t *testing.T) {
	a, b := NewSetupToken(), NewSetupToken()
	if len(a) != 43 || a == b {
		t.Fatalf("setup tokens %q and %q: want two different 43-character tokens", a, b)
	}
}
