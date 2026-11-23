package server

import (
	"fmt"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/access"
)

// The offline code names the account it acts for on every request it makes (downloads, the
// checks that a copy may still be kept, the queue's), and the server refuses any other session on
// any /api route, not only the apps': nothing is read as the wrong person, and the answer says
// nothing of what they may see.
func TestARequestNamingAnotherAccountIsRefusedOnEveryAPIRoute(t *testing.T) {
	s := newStack(t)
	as := func(cookie string, who int64) func(*http.Request) {
		return func(r *http.Request) {
			sameOrigin(cookie)(r)
			r.Header.Set(access.ExpectedUserHeader, fmt.Sprint(who))
		}
	}
	routes := []string{
		// Space metadata (the revalidation asks it), the spaces' list, a private space of someone else's.
		fmt.Sprintf("/api/workspaces/%d/spaces/%d", s.one, s.team),
		fmt.Sprintf("/api/workspaces/%d/spaces", s.one),
		fmt.Sprintf("/api/workspaces/%d/spaces/%d", s.one, s.alice.private),
		fmt.Sprintf("/api/workspaces/%d/spaces/%d/apps/1/offline", s.one, s.team),
		"/api/me",
	}
	for _, p := range routes {
		// Bob's session, the queue (or download) was alice's.
		rec := s.request(http.MethodGet, p, nil, as(s.bob.cookie, s.alice.id))
		if rec.Code != http.StatusConflict || decode(rec)["code"] != "session_account_changed" {
			t.Errorf("GET %s as bob for alice = %d %s, want 409 session_account_changed", p, rec.Code, rec.Body)
		}
	}
	// Named for the session's own account the request is as it always was.
	rec := s.request(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces/%d", s.one, s.team), nil, as(s.bob.cookie, s.bob.id))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET space for the signed-in account = %d %s", rec.Code, rec.Body)
	}
	// Without a session it is the usual 401, and a value that is no account's id is a 400.
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
	// A request without the header is untouched.
	if code, _ := s.browser(s.bob.cookie, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces", s.one), nil); code != http.StatusOK {
		t.Errorf("no header = %d, want 200", code)
	}
}

// A sign-out that names the account it is for is refused for any other session, so a page that went
// stale (another account signed in on the browser since) never signs the new one out.
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
	// Named for the session's own account, it signs out.
	rec = s.request(http.MethodPost, "/api/logout", nil, as(s.bob.cookie, s.bob.id))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout as bob for bob = %d %s, want 204", rec.Code, rec.Body)
	}
	if code, _ := s.browser(s.bob.cookie, http.MethodGet, "/api/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("bob's session after his logout: GET /api/me = %d, want 401", code)
	}
}
