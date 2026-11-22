package auth

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestValidLocale(t *testing.T) {
	for _, tag := range []string{"", "en", "pt-BR", "zh-Hant-TW", "sr-Latn", "es-419", "fil"} {
		if !ValidLocale(tag) {
			t.Errorf("ValidLocale(%q) = false", tag)
		}
	}
	for _, tag := range []string{"pt_BR", "pt-br", "PT", "english", "en-", "-en", "en-US-x-private", "zh-hant", "e", "en\n"} {
		if ValidLocale(tag) {
			t.Errorf("ValidLocale(%q) = true", tag)
		}
	}
}

func TestLocales(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")

	// None picked: "" for both, until someone picks one.
	got := decode[me](t, member.do(http.MethodGet, "/api/me", nil))
	if got.User.Locale != "" || got.Workspaces[0].Locale != "" {
		t.Fatalf("GET /api/me = %+v, want no locales", got)
	}

	// A user picks their own; PATCH /api/me answers like GET /api/me.
	got = decode[me](t, member.do(http.MethodPatch, "/api/me", map[string]string{"locale": "pt-BR"}))
	if got.User.Locale != "pt-BR" || got.User.Email != "member@example.com" {
		t.Fatalf("PATCH /api/me = %+v", got)
	}
	if got := decode[me](t, member.do(http.MethodGet, "/api/me", nil)); got.User.Locale != "pt-BR" {
		t.Fatalf("GET /api/me after the change = %+v", got)
	}
	// It is theirs alone: the owner's is untouched, and the members list shows nobody's.
	if got := decode[me](t, owner.do(http.MethodGet, "/api/me", nil)); got.User.Locale != "" {
		t.Fatalf("the owner's locale = %q after a member picked one", got.User.Locale)
	}
	rec := owner.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", ws), nil)
	expect(t, rec, http.StatusOK)
	if body := rec.Body.String(); strings.Contains(body, "locale") {
		t.Fatalf("the members list shows locales: %s", body)
	}

	// An empty body changes nothing; "" goes back to the browser's.
	if got := decode[me](t, member.do(http.MethodPatch, "/api/me", map[string]string{})); got.User.Locale != "pt-BR" {
		t.Fatalf("PATCH /api/me {} changed the locale to %q", got.User.Locale)
	}
	if got := decode[me](t, member.do(http.MethodPatch, "/api/me", map[string]string{"locale": ""})); got.User.Locale != "" {
		t.Fatalf("PATCH /api/me {locale: \"\"} left %q", got.User.Locale)
	}

	// Malformed tags and unknown fields are refused, and change nothing.
	for _, body := range []any{
		map[string]string{"locale": "pt_BR"},
		map[string]string{"locale": "<script>"},
		map[string]any{"locale": 1},
		map[string]string{"name": "Renamed"},
	} {
		expect(t, member.do(http.MethodPatch, "/api/me", body), http.StatusBadRequest)
		expect(t, owner.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d", ws), body), http.StatusBadRequest)
	}

	// The workspace's default: its admins set it, and every member sees it.
	path := fmt.Sprintf("/api/workspaces/%d", ws)
	expect(t, member.do(http.MethodPatch, path, map[string]string{"locale": "pt-BR"}), http.StatusForbidden)
	// Nobody signed in changes either.
	expect(t, e.client().do(http.MethodPatch, path, map[string]string{"locale": "pt-BR"}), http.StatusUnauthorized)
	expect(t, e.client().do(http.MethodPatch, "/api/me", map[string]string{"locale": "pt-BR"}), http.StatusUnauthorized)
	m := decode[Membership](t, owner.do(http.MethodPatch, path, map[string]string{"locale": "pt-BR"}))
	if m.WorkspaceID != ws || m.Locale != "pt-BR" || m.Role != RoleAdmin {
		t.Fatalf("PATCH %s = %+v", path, m)
	}
	if got := decode[me](t, member.do(http.MethodGet, "/api/me", nil)); got.Workspaces[0].Locale != "pt-BR" || got.User.Locale != "" {
		t.Fatalf("a member's GET /api/me = %+v, want the workspace's pt-BR and their own none", got)
	}
}
