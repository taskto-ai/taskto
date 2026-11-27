package auth

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

const newPassword = "fake-new-password"

func (e *env) userID(c *client) int64 {
	e.t.Helper()
	rec := c.do(http.MethodGet, "/api/me", nil)
	expect(e.t, rec, http.StatusOK)
	return decode[me](e.t, rec).User.ID
}

func (e *env) login(email, password string) (*client, int) {
	e.t.Helper()
	c := e.client()
	rec := c.do(http.MethodPost, "/api/login", map[string]string{"email": email, "password": password})
	return c, rec.Code
}

func (e *env) accessToken(c *client, workspace int64) string {
	e.t.Helper()
	rec := c.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/tokens", workspace), map[string]string{"name": "laptop"})
	expect(e.t, rec, http.StatusCreated)
	return decode[struct {
		Token string `json:"token"`
	}](e.t, rec).Token
}

func (e *env) withToken(token, method, path string, body any) int {
	e.t.Helper()
	r := e.client().req(method, path, body)
	r.Header.Set("Authorization", "Bearer "+token)
	return e.client().send(r).Code
}

func (e *env) reset(admin *client, workspace, userID int64) string {
	e.t.Helper()
	rec := admin.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/password-reset", workspace, userID), nil)
	expect(e.t, rec, http.StatusCreated)
	return decode[struct {
		Token string `json:"token"`
	}](e.t, rec).Token
}

func TestChangePassword(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := e.userID(owner)
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	laptop, code := e.login("owner@example.com", ownerPassword)
	if code != http.StatusOK {
		t.Fatalf("second login: %d", code)
	}
	token := e.accessToken(owner, ws)
	before := *owner.cookie
	change := func(c *client, current, next string) int {
		return c.do(http.MethodPost, "/api/me/password", map[string]string{"current_password": current, "new_password": next}).Code
	}

	for _, bad := range []struct {
		current, next string
		status        int
	}{
		{"", newPassword, http.StatusBadRequest},
		{ownerPassword, "short", http.StatusBadRequest},
		{"not the password", newPassword, http.StatusForbidden},
	} {
		if got := change(owner, bad.current, bad.next); got != bad.status {
			t.Errorf("change(%q, %q) = %d, want %d", bad.current, bad.next, got, bad.status)
		}
	}
	expect(t, laptop.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	if _, code := e.login("owner@example.com", ownerPassword); code != http.StatusOK {
		t.Fatalf("the old password stopped working after refused changes: %d", code)
	}

	if got := change(owner, ownerPassword, newPassword); got != http.StatusNoContent {
		t.Fatalf("change = %d", got)
	}
	if owner.cookie == nil || owner.cookie.Value == before.Value {
		t.Fatal("the browser that changed the password didn't get a new session")
	}
	expect(t, owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	expect(t, laptop.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	stale := e.client()
	stale.cookie = &before
	expect(t, stale.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	if n := e.count(`SELECT count(*) FROM sessions WHERE user_id = ?`, ownerID); n != 1 {
		t.Errorf("%d sessions after the change, want 1", n)
	}
	if _, code := e.login("owner@example.com", ownerPassword); code != http.StatusUnauthorized {
		t.Errorf("the old password: %d", code)
	}
	if _, code := e.login("owner@example.com", newPassword); code != http.StatusOK {
		t.Errorf("the new password: %d", code)
	}
	expect(t, member.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	if _, code := e.login("member@example.com", otherPassword); code != http.StatusOK {
		t.Errorf("the member's password after the owner's change: %d", code)
	}
	if code := e.withToken(token, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", ws), nil); code != http.StatusOK {
		t.Errorf("an access token after the change: %d", code)
	}
}

func TestChangePasswordEndsResetLinks(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	link := e.reset(owner, ws, e.userID(member))
	expect(t, member.do(http.MethodPost, "/api/me/password", map[string]string{"current_password": otherPassword, "new_password": newPassword}), http.StatusNoContent)
	expect(t, e.client().do(http.MethodPost, "/api/password-reset", map[string]string{"token": link, "password": "fake-third-password"}), http.StatusNotFound)
}

func TestChangePasswordActsOnTheCallerOnly(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := e.userID(owner)
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")

	for _, field := range []string{"user_id", "user", "email"} {
		body := map[string]any{"current_password": otherPassword, "new_password": newPassword, field: ownerID}
		if field == "email" {
			body[field] = "owner@example.com"
		}
		expect(t, member.do(http.MethodPost, "/api/me/password", body), http.StatusBadRequest)
	}
	expect(t, member.do(http.MethodPost, "/api/me/password", map[string]string{"current_password": ownerPassword, "new_password": newPassword}), http.StatusForbidden)
	expect(t, member.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/password", ws, ownerID), map[string]string{"new_password": newPassword}), http.StatusNotFound)

	expect(t, owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	if _, code := e.login("owner@example.com", ownerPassword); code != http.StatusOK {
		t.Fatalf("the owner's password changed: login %d", code)
	}
	if _, code := e.login("member@example.com", otherPassword); code != http.StatusOK {
		t.Fatalf("the member's password changed: login %d", code)
	}

	body := map[string]string{"current_password": ownerPassword, "new_password": newPassword}
	expect(t, e.client().do(http.MethodPost, "/api/me/password", body), http.StatusUnauthorized)
	if code := e.withToken(e.accessToken(owner, ws), http.MethodPost, "/api/me/password", body); code != http.StatusForbidden {
		t.Errorf("an access token changed the password: %d", code)
	}
	if _, code := e.login("owner@example.com", ownerPassword); code != http.StatusOK {
		t.Fatalf("the owner's password changed: login %d", code)
	}
}

func TestChangePasswordIsLimited(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	body := map[string]string{"current_password": "not the password", "new_password": newPassword}
	for i := range accountAndIPAttempts {
		if code := owner.do(http.MethodPost, "/api/me/password", body).Code; code != http.StatusForbidden {
			t.Fatalf("attempt %d: %d", i+1, code)
		}
	}
	rec := owner.do(http.MethodPost, "/api/me/password", body)
	expect(t, rec, http.StatusTooManyRequests)
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	right := map[string]string{"current_password": ownerPassword, "new_password": newPassword}
	expect(t, owner.do(http.MethodPost, "/api/me/password", right), http.StatusTooManyRequests)
	e.advance(attemptWindow)
	expect(t, owner.do(http.MethodPost, "/api/me/password", right), http.StatusNoContent)
}

func TestResetLink(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	memberID := e.userID(member)
	path := fmt.Sprintf("/api/workspaces/%d/members/%d/password-reset", ws, memberID)
	use := func(c *client, token, password string) int {
		return c.do(http.MethodPost, "/api/password-reset", map[string]string{"token": token, "password": password}).Code
	}

	rec := owner.do(http.MethodPost, path, nil)
	expect(t, rec, http.StatusCreated)
	made := decode[struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}](t, rec)
	if made.Token == "" || made.ExpiresAt != stamp(e.now.Add(24*time.Hour)) {
		t.Fatalf("made %+v", made)
	}
	if n := e.count(`SELECT count(*) FROM password_resets WHERE token_hash = ?`, made.Token); n != 0 {
		t.Fatal("the token is stored as is")
	}
	first := made.Token
	second := e.reset(owner, ws, memberID)
	expect(t, e.client().do(http.MethodPost, "/api/password-reset", map[string]string{"token": first, "password": newPassword}), http.StatusNotFound)

	browser := e.client()
	if code := use(browser, second, "short"); code != http.StatusBadRequest {
		t.Fatalf("a short password: %d", code)
	}
	if code := use(browser, "not-a-token", newPassword); code != http.StatusNotFound {
		t.Fatalf("an unknown token: %d", code)
	}
	if code := use(browser, second, newPassword); code != http.StatusOK {
		t.Fatalf("using the link: %d", code)
	}
	if got := e.userID(browser); got != memberID {
		t.Fatalf("signed in as %d, not the member %d", got, memberID)
	}
	expect(t, member.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	expect(t, owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	if _, code := e.login("member@example.com", otherPassword); code != http.StatusUnauthorized {
		t.Errorf("the old password: %d", code)
	}
	if _, code := e.login("member@example.com", newPassword); code != http.StatusOK {
		t.Errorf("the new password: %d", code)
	}
	if _, code := e.login("owner@example.com", ownerPassword); code != http.StatusOK {
		t.Errorf("the owner's password after the member's reset: %d", code)
	}
	if code := use(e.client(), second, "fake-third-password"); code != http.StatusNotFound {
		t.Errorf("the link a second time: %d", code)
	}

	third := e.reset(owner, ws, memberID)
	e.advance(24*time.Hour + time.Second)
	if code := use(e.client(), third, "fake-third-password"); code != http.StatusNotFound {
		t.Errorf("an expired link: %d", code)
	}
	if _, code := e.login("member@example.com", newPassword); code != http.StatusOK {
		t.Errorf("an expired link changed the password: %d", code)
	}
}

func TestResetLinkAccess(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := e.userID(owner)
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	memberID := e.userID(member)
	other, _ := e.join(e.invite(owner, ws, "other@example.com", RoleMember), "Other")
	otherID := e.userID(other)
	outsider, outsiderMe := e.join(e.invite(owner, ws, "outsider@example.com", RoleMember), "Outsider")
	elsewhere := e.addWorkspace(outsiderMe.User.ID, "Elsewhere")
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, outsiderMe.User.ID), nil), http.StatusNoContent)
	path := func(workspace, user int64) string {
		return fmt.Sprintf("/api/workspaces/%d/members/%d/password-reset", workspace, user)
	}

	for _, target := range []int64{ownerID, otherID, memberID} {
		expect(t, member.do(http.MethodPost, path(ws, target), nil), http.StatusForbidden)
	}
	expect(t, e.client().do(http.MethodPost, path(ws, memberID), nil), http.StatusUnauthorized)
	expect(t, outsider.do(http.MethodPost, path(ws, memberID), nil), http.StatusNotFound)
	expect(t, outsider.do(http.MethodPost, path(elsewhere, memberID), nil), http.StatusNotFound)
	expect(t, owner.do(http.MethodPost, path(ws, outsiderMe.User.ID), nil), http.StatusNotFound)
	expect(t, owner.do(http.MethodPost, path(ws, 999), nil), http.StatusNotFound)
	expect(t, owner.do(http.MethodPost, path(ws, ownerID), nil), http.StatusBadRequest)
	expect(t, owner.do(http.MethodPost, path(ws, memberID), map[string]any{"user_id": otherID}), http.StatusBadRequest)
	if code := e.withToken(e.accessToken(owner, ws), http.MethodPost, path(ws, memberID), nil); code != http.StatusForbidden {
		t.Errorf("an access token made a reset link: %d", code)
	}
	if n := e.count(`SELECT count(*) FROM password_resets`); n != 0 {
		t.Fatalf("%d reset links after refused calls", n)
	}

	theirs := e.addWorkspace(memberID, "The member's own")
	expect(t, owner.do(http.MethodPost, path(ws, memberID), nil), http.StatusForbidden)
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, theirs, ownerID); err != nil {
		t.Fatal(err)
	}
	expect(t, owner.do(http.MethodPost, path(ws, memberID), nil), http.StatusForbidden)
	if _, err := e.db.Exec(`UPDATE memberships SET role = 'admin' WHERE workspace_id = ? AND user_id = ?`, theirs, ownerID); err != nil {
		t.Fatal(err)
	}
	e.reset(owner, ws, memberID)

	if _, err := e.db.Exec(`INSERT INTO invites (workspace_id, token_hash, email, role, expires_at) VALUES (?, ?, 'other@example.com', 'admin', ?)`,
		elsewhere, hashToken("a pending invite"), stamp(e.now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}

	link := e.reset(owner, ws, otherID)
	r := e.client().req(http.MethodPost, "/api/password-reset", map[string]string{"token": link, "password": newPassword})
	r.Header.Set("Authorization", "Bearer "+e.accessToken(owner, ws))
	if code := e.client().send(r).Code; code != http.StatusForbidden {
		t.Errorf("a reset with an access token: %d", code)
	}
	if _, code := e.login("other@example.com", otherPassword); code != http.StatusOK {
		t.Errorf("a refused reset changed the password: %d", code)
	}
}
