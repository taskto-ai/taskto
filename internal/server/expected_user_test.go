package server

import (
	"fmt"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/access"
)

func TestARequestNamingAnotherAccountIsRefusedOnEveryAPIRoute(t *testing.T) {
	s := newStack(t)
	as := func(cookie string, who int64) func(*http.Request) {
		return func(r *http.Request) {
			sameOrigin(cookie)(r)
			r.Header.Set(access.ExpectedUserHeader, fmt.Sprint(who))
		}
	}
	routes := []string{
		fmt.Sprintf("/api/workspaces/%d/spaces/%d", s.one, s.team),
		fmt.Sprintf("/api/workspaces/%d/spaces", s.one),
		fmt.Sprintf("/api/workspaces/%d/spaces/%d", s.one, s.alice.private),
		fmt.Sprintf("/api/workspaces/%d/spaces/%d/apps/1/offline", s.one, s.team),
		"/api/me",
	}
	for _, p := range routes {
		rec := s.request(http.MethodGet, p, nil, as(s.bob.cookie, s.alice.id))
		if rec.Code != http.StatusConflict || decode(rec)["code"] != "session_account_changed" {
			t.Errorf("GET %s as bob for alice = %d %s, want 409 session_account_changed", p, rec.Code, rec.Body)
		}
	}
	rec := s.request(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces/%d", s.one, s.team), nil, as(s.bob.cookie, s.bob.id))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET space for the signed-in account = %d %s", rec.Code, rec.Body)
	}
	rec = s.request(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces", s.one), nil, as("", s.alice.id))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no session = %d, want 401", rec.Code)
	}
	rec = s.request(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces", s.one), nil, func(r *http.Request) {
		sameOrigin(s.bob.cookie)(r)
		r.Header.Set(access.ExpectedUserHeader, "someone")
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a value that is no account = %d, want 400", rec.Code)
	}
	if code, _ := s.browser(s.bob.cookie, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces", s.one), nil); code != http.StatusOK {
		t.Errorf("no header = %d, want 200", code)
	}
}

func TestALogoutNamingAnotherAccountLeavesTheSessionAlone(t *testing.T) {
	s := newStack(t)
	as := func(cookie string, who int64) func(*http.Request) {
		return func(r *http.Request) {
			sameOrigin(cookie)(r)
			r.Header.Set(access.ExpectedUserHeader, fmt.Sprint(who))
		}
	}
	rec := s.request(http.MethodPost, "/api/logout", nil, as(s.bob.cookie, s.alice.id))
	if rec.Code != http.StatusConflict || decode(rec)["code"] != "session_account_changed" {
		t.Fatalf("logout as bob for alice = %d %s, want 409 session_account_changed", rec.Code, rec.Body)
	}
	if code, _ := s.browser(s.bob.cookie, http.MethodGet, "/api/me", nil); code != http.StatusOK {
		t.Fatalf("bob's session after the refused logout: GET /api/me = %d, want 200", code)
	}
	rec = s.request(http.MethodPost, "/api/logout", nil, as(s.bob.cookie, s.bob.id))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout as bob for bob = %d %s, want 204", rec.Code, rec.Body)
	}
	if code, _ := s.browser(s.bob.cookie, http.MethodGet, "/api/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("bob's session after his logout: GET /api/me = %d, want 401", code)
	}
	rec = s.request(http.MethodPost, "/api/logout", map[string]string{"session": "any"}, as(s.bob.cookie, s.bob.id))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("named logout of an over login = %d %s, want 401", rec.Code, rec.Body)
	}
	rec = s.request(http.MethodPost, "/api/logout", map[string]string{"session": "any"}, sameOrigin(s.bob.cookie))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout of an over login without the header = %d %s, want 204", rec.Code, rec.Body)
	}
}
