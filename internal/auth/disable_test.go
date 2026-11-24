package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
)

func (e *env) disablePath(ws, user int64, verb string) string {
	return fmt.Sprintf("/api/workspaces/%d/members/%d/%s", ws, user, verb)
}

// team is an owner (admin) and a member who has a laptop and a phone logged in and an access
// token, all working.
type team struct {
	e        *env
	owner    *client
	ws       int64
	member   *client
	phone    *client
	memberID int64
	ownerID  int64
	token    string
}

func newTeam(t *testing.T, opts Options) *team {
	t.Helper()
	e := newEnv(t, opts)
	owner, ws := e.setup()
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	phone, code := e.login("member@example.com", otherPassword)
	if code != http.StatusOK {
		t.Fatalf("the member's second login: %d", code)
	}
	tm := &team{e: e, owner: owner, ws: ws, member: member, phone: phone,
		memberID: e.userID(member), ownerID: e.userID(owner), token: e.accessToken(member, ws)}
	if code := e.withToken(tm.token, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", ws), nil); code != http.StatusOK {
		t.Fatalf("the token before: %d", code)
	}
	return tm
}

func (tm *team) disable() {
	tm.e.t.Helper()
	expect(tm.e.t, tm.owner.do(http.MethodPost, tm.e.disablePath(tm.ws, tm.memberID, "disable"), nil), http.StatusNoContent)
}

func (tm *team) enable() {
	tm.e.t.Helper()
	expect(tm.e.t, tm.owner.do(http.MethodPost, tm.e.disablePath(tm.ws, tm.memberID, "enable"), nil), http.StatusNoContent)
}

func TestDisableSignsTheMemberOutEverywhereAndRefusesPasswordLogin(t *testing.T) {
	tm := newTeam(t, Options{})
	e := tm.e
	tm.disable()

	// Every session of theirs is gone at once, and no session of the owner's is touched.
	expect(t, tm.member.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	expect(t, tm.phone.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	expect(t, tm.owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	if n := e.count(`SELECT count(*) FROM sessions WHERE user_id = ?`, tm.memberID); n != 0 {
		t.Fatalf("%d sessions of the disabled account remain", n)
	}

	// The right password is refused, and says why; a wrong one is a wrong one, so nobody who
	// doesn't know the password learns that the account is off.
	c := e.client()
	rec := c.do(http.MethodPost, "/api/login", map[string]string{"email": "member@example.com", "password": otherPassword})
	expect(t, rec, http.StatusForbidden)
	if got := decode[struct{ Code string }](t, rec).Code; got != "account_disabled" {
		t.Fatalf("code = %q", got)
	}
	if c.cookie != nil {
		t.Fatal("a refused login set a session cookie")
	}
	rec = e.client().do(http.MethodPost, "/api/login", map[string]string{"email": "member@example.com", "password": "not the password"})
	expect(t, rec, http.StatusUnauthorized)
	if n := e.count(`SELECT count(*) FROM sessions WHERE user_id = ?`, tm.memberID); n != 0 {
		t.Fatal("a session was made for a disabled account")
	}

	// What they own stays.
	if n := e.count(`SELECT count(*) FROM memberships WHERE user_id = ?`, tm.memberID); n != 1 {
		t.Fatalf("memberships = %d, want the one kept", n)
	}
	if n := e.count(`SELECT count(*) FROM access_tokens WHERE user_id = ?`, tm.memberID); n != 1 {
		t.Fatalf("access tokens = %d, want the one kept", n)
	}
}

func TestEnableRestoresAccess(t *testing.T) {
	tm := newTeam(t, Options{})
	e := tm.e
	tm.disable()
	tm.enable()

	c, code := e.login("member@example.com", otherPassword)
	if code != http.StatusOK {
		t.Fatalf("login after enabling: %d", code)
	}
	if got := e.userID(c); got != tm.memberID {
		t.Fatalf("logged in as %d", got)
	}
	// The member is the same: the same membership, and the token they made before works again.
	if code := e.withToken(tm.token, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", tm.ws), nil); code != http.StatusOK {
		t.Fatalf("the token after enabling: %d", code)
	}
	// The sessions that were signed out stay signed out: enabling is not undoing the sign-out.
	expect(t, tm.member.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
}

func TestDisabledAccessTokensAreRefused(t *testing.T) {
	tm := newTeam(t, Options{})
	tm.disable()
	path := fmt.Sprintf("/api/workspaces/%d/members", tm.ws)
	if code := tm.e.withToken(tm.token, http.MethodGet, path, nil); code != http.StatusUnauthorized {
		t.Fatalf("a disabled account's token: %d, want 401", code)
	}
	if _, _, err := tm.e.svc.TokenUser(context.Background(), tm.token); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("TokenUser = %v, want ErrTokenInvalid (what MCP asks)", err)
	}
	// The owner's own token is unaffected.
	other := tm.e.accessToken(tm.owner, tm.ws)
	if code := tm.e.withToken(other, http.MethodGet, path, nil); code != http.StatusOK {
		t.Fatalf("another account's token: %d", code)
	}
}

func TestDisabledAccountCantUseAResetLink(t *testing.T) {
	tm := newTeam(t, Options{})
	e := tm.e
	before := e.reset(tm.owner, tm.ws, tm.memberID)
	tm.disable()
	use := func(token string) (int, string) {
		rec := e.client().do(http.MethodPost, "/api/password-reset", map[string]string{"token": token, "password": newPassword})
		return rec.Code, decode[struct{ Code string }](t, rec).Code
	}
	// The link made before is gone with the sign-out.
	if code, c := use(before); code != http.StatusNotFound || c != "reset_invalid" {
		t.Fatalf("a link from before: %d %s", code, c)
	}
	// An admin may still make one (it is for after the account is enabled), but it sets nothing
	// and signs nobody in meanwhile.
	after := e.reset(tm.owner, tm.ws, tm.memberID)
	if code, c := use(after); code != http.StatusForbidden || c != "account_disabled" {
		t.Fatalf("a link for a disabled account: %d %s", code, c)
	}
	if n := e.count(`SELECT count(*) FROM sessions WHERE user_id = ?`, tm.memberID); n != 0 {
		t.Fatal("a reset link signed a disabled account in")
	}
	tm.enable()
	if _, code := e.login("member@example.com", otherPassword); code != http.StatusOK {
		t.Fatalf("the password changed although the link was refused: %d", code)
	}
}

func TestDisabledAccountCantSignInWithAProvider(t *testing.T) {
	s := newSSOEnv(t)
	owner, ws := s.setup()
	// A member with a linked identity, and an account never signed in with a provider.
	member, _ := s.join(s.invite(owner, ws, "member@example.com", RoleMember), "Member")
	memberID := s.userID(member)
	other, _ := s.join(s.invite(owner, ws, "other@example.com", RoleMember), "Other")
	otherID := s.userID(other)

	b := s.browser()
	s.wantLogin(b, s.signIn(b, "/boards", verified("member@example.com")), "/boards", "member@example.com")

	for _, id := range []int64{memberID, otherID} {
		expect(t, owner.do(http.MethodPost, s.disablePath(ws, id, "disable"), nil), http.StatusNoContent)
	}
	linked := s.count(`SELECT count(*) FROM user_identities`)

	// The linked identity, and a first sign-in that would link by email: both refused, and the
	// second links nothing.
	b2 := s.browser()
	s.wantFailure(s.signIn(b2, "/boards", verified("member@example.com")), apierr.CodeAccountDisabled)
	s.wantFailure(s.signIn(b2, "/boards", map[string]any{"email": "other@example.com", "email_verified": true, "name": "Other", "sub": "sub-other"}),
		apierr.CodeAccountDisabled)
	expect(t, b2.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	if n := s.count(`SELECT count(*) FROM user_identities`); n != linked {
		t.Fatalf("identities = %d, want %d: a refused sign-in linked one", n, linked)
	}
	if n := s.count(`SELECT count(*) FROM sessions WHERE user_id IN (?, ?)`, memberID, otherID); n != 0 {
		t.Fatalf("%d sessions for disabled accounts", n)
	}
	rows := s.events(audit.SSOLoginFailed)
	if len(rows) < 2 {
		t.Fatalf("sso_login_failed events = %d, want a record of each refusal", len(rows))
	}

	// Enabled again, the same sign-in works.
	expect(t, owner.do(http.MethodPost, s.disablePath(ws, memberID, "enable"), nil), http.StatusNoContent)
	b3 := s.browser()
	s.wantLogin(b3, s.signIn(b3, "/boards", verified("member@example.com")), "/boards", "member@example.com")
}

func TestDisableMakesNoSessionBySomeOtherWay(t *testing.T) {
	tm := newTeam(t, Options{})
	tm.disable()
	// Every new session is made by insertSession: it refuses a disabled account, whoever asks.
	err := tm.e.svc.write(context.Background(), func(tx *sql.Tx) error {
		_, err := tm.e.svc.insertSession(context.Background(), tx, tm.memberID)
		return err
	})
	if !errors.Is(err, errAccountDisabled) {
		t.Fatalf("insertSession = %v, want errAccountDisabled", err)
	}
	tm.enable()
	if err := tm.e.svc.write(context.Background(), func(tx *sql.Tx) error {
		_, err := tm.e.svc.insertSession(context.Background(), tx, tm.memberID)
		return err
	}); err != nil {
		t.Fatalf("insertSession after enabling: %v", err)
	}
}

func TestMembersShowWhoIsDisabled(t *testing.T) {
	tm := newTeam(t, Options{})
	list := func(c *client) map[int64]bool {
		rec := c.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", tm.ws), nil)
		expect(t, rec, http.StatusOK)
		out := map[int64]bool{}
		for _, m := range decode[struct{ Members []Member }](t, rec).Members {
			out[m.ID] = m.Disabled
		}
		return out
	}
	if got := list(tm.owner); got[tm.memberID] || got[tm.ownerID] {
		t.Fatalf("before: %v", got)
	}
	tm.disable()
	if got := list(tm.owner); !got[tm.memberID] || got[tm.ownerID] {
		t.Fatalf("after: %v", got)
	}
	tm.enable()
	if got := list(tm.owner); got[tm.memberID] {
		t.Fatalf("after enabling: %v", got)
	}
}

func TestDisableIsIdempotent(t *testing.T) {
	tm := newTeam(t, Options{})
	tm.disable()
	tm.disable()
	if n := len(tm.e.events(audit.MemberDisabled)); n != 1 {
		t.Fatalf("member_disabled events = %d after disabling twice", n)
	}
	var at string
	if err := tm.e.db.QueryRow(`SELECT disabled_at FROM users WHERE id = ?`, tm.memberID).Scan(&at); err != nil || at == "" {
		t.Fatalf("disabled_at = %q, %v", at, err)
	}
	tm.enable()
	tm.enable()
	if n := len(tm.e.events(audit.MemberEnabled)); n != 1 {
		t.Fatalf("member_enabled events = %d after enabling twice", n)
	}
}

func TestDisableAndEnableAreAudited(t *testing.T) {
	tm := newTeam(t, Options{})
	tm.disable()
	ev := tm.e.one(audit.MemberDisabled)
	wantEvent(t, ev, tm.ws, tm.ownerID, audit.TargetUser, tm.memberID)
	if got := ev.Details["logins_ended"]; fmt.Sprint(got) != "2" {
		t.Errorf("logins_ended = %v, want the member's 2", got)
	}
	// A refused login is an event too, with its reason.
	tm.e.login("member@example.com", otherPassword)
	failed := tm.e.events(audit.LoginFailed)
	if len(failed) != 1 || fmt.Sprint(failed[0].Details["reason"]) != "account_disabled" {
		t.Fatalf("login_failed = %+v", failed)
	}
	wantID(t, "login_failed target", failed[0].TargetID, tm.memberID)
	tm.enable()
	wantEvent(t, tm.e.one(audit.MemberEnabled), tm.ws, tm.ownerID, audit.TargetUser, tm.memberID)
}

// The refusals. None of them changes the account or records an event.
func TestDisableRefusals(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := e.userID(owner)
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	memberID := e.userID(member)
	second, _ := e.join(e.invite(owner, ws, "admin2@example.com", RoleAdmin), "Admin Two")
	secondID := e.userID(second)
	// An admin of another workspace altogether, who isn't in this one.
	outsider, _ := e.join(e.invite(owner, ws, "elsewhere@example.com", RoleMember), "Elsewhere")
	outsiderID := e.userID(outsider)
	e.addWorkspace(outsiderID, "Other")
	if _, err := e.db.Exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, ws, outsiderID); err != nil {
		t.Fatal(err)
	}

	check := func(name string, c *client, path string, status int, code apierr.Code) {
		t.Helper()
		rec := c.do(http.MethodPost, path, nil)
		if rec.Code != status {
			t.Fatalf("%s: status %d, want %d (%s)", name, rec.Code, status, rec.Body.String())
		}
		if code != "" {
			if got := decode[struct{ Code string }](t, rec).Code; got != string(code) {
				t.Fatalf("%s: code %q, want %q", name, got, code)
			}
		}
	}

	// Not an admin; not logged in; a token (a session only).
	check("a member", member, e.disablePath(ws, secondID, "disable"), http.StatusForbidden, apierr.CodeAdminOnly)
	check("a member enabling", member, e.disablePath(ws, secondID, "enable"), http.StatusForbidden, apierr.CodeAdminOnly)
	check("no login", e.client(), e.disablePath(ws, memberID, "disable"), http.StatusUnauthorized, apierr.CodeNotLoggedIn)
	token := e.accessToken(owner, ws)
	for _, verb := range []string{"disable", "enable"} {
		r := e.client().req(http.MethodPost, e.disablePath(ws, memberID, verb), nil)
		r.Header.Set("Authorization", "Bearer "+token)
		if rec := e.client().send(r); rec.Code != http.StatusForbidden {
			t.Fatalf("a token to %s: %d", verb, rec.Code)
		}
	}
	// Not an admin of the workspace in the path: the workspace isn't there for them.
	check("an outsider", outsider, e.disablePath(ws, memberID, "disable"), http.StatusNotFound, apierr.CodeWorkspaceNotFound)

	// Your own account.
	check("own account", owner, e.disablePath(ws, ownerID, "disable"), http.StatusBadRequest, apierr.CodeDisableOwn)
	check("own account, enabling", owner, e.disablePath(ws, ownerID, "enable"), http.StatusBadRequest, apierr.CodeDisableOwn)

	// Not a member of this workspace, and not a user at all.
	check("not a member of the workspace", owner, e.disablePath(ws, 9999, "disable"), http.StatusNotFound, apierr.CodeMemberNotFound)
	check("a bad id", owner, fmt.Sprintf("/api/workspaces/%d/members/x/disable", ws), http.StatusNotFound, apierr.CodeMemberNotFound)
	check("a workspace that isn't theirs", owner, e.disablePath(9999, memberID, "disable"), http.StatusNotFound, apierr.CodeWorkspaceNotFound)

	// A body is refused: the route takes what's in its path.
	rec := owner.do(http.MethodPost, e.disablePath(ws, memberID, "disable"), map[string]string{"x": "y"})
	expect(t, rec, http.StatusBadRequest)

	// Someone who belongs to a workspace the admin doesn't administer.
	e.addMembership(t, e.addWorkspace(secondID, "Seconds"), memberID, RoleMember)
	check("a member of another workspace too", owner, e.disablePath(ws, memberID, "disable"), http.StatusForbidden, apierr.CodeDisableElsewhere)
	check("the same, enabling", owner, e.disablePath(ws, memberID, "enable"), http.StatusForbidden, apierr.CodeDisableElsewhere)

	// Nothing changed, and nothing was recorded.
	if n := e.count(`SELECT count(*) FROM users WHERE disabled_at IS NOT NULL`); n != 0 {
		t.Fatalf("%d accounts are disabled after only refusals", n)
	}
	if n := len(e.events(audit.MemberDisabled)) + len(e.events(audit.MemberEnabled)); n != 0 {
		t.Fatalf("%d events for refusals", n)
	}
	expect(t, member.do(http.MethodGet, "/api/me", nil), http.StatusOK)
}

func (e *env) addMembership(t *testing.T, workspace, user int64, role Role) {
	t.Helper()
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, ?)`, workspace, user, role); err != nil {
		t.Fatal(err)
	}
}

// An admin who administers every workspace of the account may disable it; one who administers
// only some may not, as with a reset link.
func TestDisableNeedsAnAdminOfEveryWorkspace(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := e.userID(owner)
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	memberID := e.userID(member)
	ws2 := e.addWorkspace(ownerID, "Second")
	e.addMembership(t, ws2, memberID, RoleMember)
	// The owner administers both: allowed, whichever workspace the request names.
	expect(t, owner.do(http.MethodPost, e.disablePath(ws2, memberID, "disable"), nil), http.StatusNoContent)
	expect(t, member.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	expect(t, owner.do(http.MethodPost, e.disablePath(ws, memberID, "enable"), nil), http.StatusNoContent)
	if _, code := e.login("member@example.com", otherPassword); code != http.StatusOK {
		t.Fatalf("login after enabling from the other workspace: %d", code)
	}
}

// Two admins can't disable each other and leave the workspace with nobody: once one is off, they
// can't act, and an admin in use can't be demoted or removed from under the workspace either.
func TestAWorkspaceKeepsAnAdminInUse(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := e.userID(owner)
	second, _ := e.join(e.invite(owner, ws, "admin2@example.com", RoleAdmin), "Admin Two")
	secondID := e.userID(second)

	expect(t, owner.do(http.MethodPost, e.disablePath(ws, secondID, "disable"), nil), http.StatusNoContent)
	// The other asks the same, a moment late (its session had already passed the middleware): the
	// transaction finds it disabled.
	err := e.svc.setDisabled(context.Background(), ws, secondID, ownerID, true)
	if !errors.Is(err, errAccountDisabled) {
		t.Fatalf("setDisabled by a disabled admin = %v, want errAccountDisabled", err)
	}
	if n := e.count(`SELECT count(*) FROM users WHERE disabled_at IS NULL`); n != 1 {
		t.Fatalf("%d accounts in use, want only the owner", n)
	}

	// The disabled admin doesn't count: the owner is the last one in use.
	for _, tc := range []struct{ method, body string }{{http.MethodPatch, `member`}, {http.MethodDelete, ``}} {
		var rec = owner.do(tc.method, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, ownerID), func() any {
			if tc.body == "" {
				return nil
			}
			return map[string]string{"role": tc.body}
		}())
		expect(t, rec, http.StatusConflict)
		if got := decode[struct{ Code string }](t, rec).Code; got != "last_admin" {
			t.Fatalf("%s: code %q", tc.method, got)
		}
	}
	// The disabled admin themselves may go: the owner stays.
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, secondID), nil), http.StatusNoContent)
}
