package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
)

func (e *env) disablePath(ws, user int64, verb string) string {
	return fmt.Sprintf("/api/workspaces/%d/members/%d/%s", ws, user, verb)
}

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

	expect(t, tm.member.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	expect(t, tm.phone.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	expect(t, tm.owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	if n := e.count(`SELECT count(*) FROM sessions WHERE user_id = ?`, tm.memberID); n != 0 {
		t.Fatalf("%d sessions of the disabled account remain", n)
	}

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
	if code := e.withToken(tm.token, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", tm.ws), nil); code != http.StatusOK {
		t.Fatalf("the token after enabling: %d", code)
	}
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
	if code, c := use(before); code != http.StatusNotFound || c != "reset_invalid" {
		t.Fatalf("a link from before: %d %s", code, c)
	}
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

	expect(t, owner.do(http.MethodPost, s.disablePath(ws, memberID, "enable"), nil), http.StatusNoContent)
	b3 := s.browser()
	s.wantLogin(b3, s.signIn(b3, "/boards", verified("member@example.com")), "/boards", "member@example.com")
}

func TestDisableMakesNoSessionBySomeOtherWay(t *testing.T) {
	tm := newTeam(t, Options{})
	tm.disable()
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
	tm.e.login("member@example.com", otherPassword)
	failed := tm.e.events(audit.LoginFailed)
	if len(failed) != 1 || fmt.Sprint(failed[0].Details["reason"]) != "account_disabled" {
		t.Fatalf("login_failed = %+v", failed)
	}
	wantID(t, "login_failed target", failed[0].TargetID, tm.memberID)
	tm.enable()
	wantEvent(t, tm.e.one(audit.MemberEnabled), tm.ws, tm.ownerID, audit.TargetUser, tm.memberID)
}

func TestDisableRefusals(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := e.userID(owner)
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	memberID := e.userID(member)
	second, _ := e.join(e.invite(owner, ws, "admin2@example.com", RoleAdmin), "Admin Two")
	secondID := e.userID(second)
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
	check("an outsider", outsider, e.disablePath(ws, memberID, "disable"), http.StatusNotFound, apierr.CodeWorkspaceNotFound)

	check("own account", owner, e.disablePath(ws, ownerID, "disable"), http.StatusBadRequest, apierr.CodeDisableOwn)
	check("own account, enabling", owner, e.disablePath(ws, ownerID, "enable"), http.StatusBadRequest, apierr.CodeDisableOwn)

	check("not a member of the workspace", owner, e.disablePath(ws, 9999, "disable"), http.StatusNotFound, apierr.CodeMemberNotFound)
	check("a bad id", owner, fmt.Sprintf("/api/workspaces/%d/members/x/disable", ws), http.StatusNotFound, apierr.CodeMemberNotFound)
	check("a workspace that isn't theirs", owner, e.disablePath(9999, memberID, "disable"), http.StatusNotFound, apierr.CodeWorkspaceNotFound)

	rec := owner.do(http.MethodPost, e.disablePath(ws, memberID, "disable"), map[string]string{"x": "y"})
	expect(t, rec, http.StatusBadRequest)

	e.addMembership(t, e.addWorkspace(secondID, "Seconds"), memberID, RoleMember)
	check("a member of another workspace too", owner, e.disablePath(ws, memberID, "disable"), http.StatusForbidden, apierr.CodeDisableElsewhere)
	check("the same, enabling", owner, e.disablePath(ws, memberID, "enable"), http.StatusForbidden, apierr.CodeDisableElsewhere)

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

func TestDisableNeedsAnAdminOfEveryWorkspace(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := e.userID(owner)
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	memberID := e.userID(member)
	ws2 := e.addWorkspace(ownerID, "Second")
	e.addMembership(t, ws2, memberID, RoleMember)
	expect(t, owner.do(http.MethodPost, e.disablePath(ws2, memberID, "disable"), nil), http.StatusNoContent)
	expect(t, member.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	expect(t, owner.do(http.MethodPost, e.disablePath(ws, memberID, "enable"), nil), http.StatusNoContent)
	if _, code := e.login("member@example.com", otherPassword); code != http.StatusOK {
		t.Fatalf("login after enabling from the other workspace: %d", code)
	}
}

func TestAWorkspaceKeepsAnAdminInUse(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := e.userID(owner)
	second, _ := e.join(e.invite(owner, ws, "admin2@example.com", RoleAdmin), "Admin Two")
	secondID := e.userID(second)

	expect(t, owner.do(http.MethodPost, e.disablePath(ws, secondID, "disable"), nil), http.StatusNoContent)
	err := e.svc.setDisabled(context.Background(), ws, secondID, ownerID, true)
	if !errors.Is(err, errAccountDisabled) {
		t.Fatalf("setDisabled by a disabled admin = %v, want errAccountDisabled", err)
	}
	if n := e.count(`SELECT count(*) FROM users WHERE disabled_at IS NULL`); n != 1 {
		t.Fatalf("%d accounts in use, want only the owner", n)
	}

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
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, secondID), nil), http.StatusNoContent)
}

func TestDisabledAdminCantMakeAResetLink(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := e.userID(owner)
	second, _ := e.join(e.invite(owner, ws, "admin2@example.com", RoleAdmin), "Admin Two")
	secondID := e.userID(second)
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	memberID := e.userID(member)

	expect(t, owner.do(http.MethodPost, e.disablePath(ws, secondID, "disable"), nil), http.StatusNoContent)
	_, _, err := e.svc.createReset(context.Background(), ws, secondID, memberID)
	if !errors.Is(err, errAccountDisabled) {
		t.Fatalf("createReset by a disabled admin = %v, want errAccountDisabled", err)
	}
	if n := e.count(`SELECT count(*) FROM password_resets WHERE user_id = ?`, memberID); n != 0 {
		t.Fatalf("%d reset links were made for the member", n)
	}
	if _, _, err := e.svc.createReset(context.Background(), ws, ownerID, memberID); err != nil {
		t.Fatalf("createReset by the owner: %v", err)
	}
}

func TestDisabledAdminChangesNothingInFlight(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	second, _ := e.join(e.invite(owner, ws, "admin2@example.com", RoleAdmin), "Admin Two")
	secondID := e.userID(second)
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	memberID := e.userID(member)
	e.invite(owner, ws, "pending@example.com", RoleMember)
	var inviteID int64
	if err := e.db.QueryRow(`SELECT id FROM invites WHERE email = 'pending@example.com'`).Scan(&inviteID); err != nil {
		t.Fatal(err)
	}
	expect(t, owner.do(http.MethodPost, e.disablePath(ws, secondID, "disable"), nil), http.StatusNoContent)
	ctx := audit.WithActor(context.Background(), secondID)

	if _, _, err := e.svc.createInvite(ctx, ws, secondID, "new@example.com", RoleMember); !errors.Is(err, errAccountDisabled) {
		t.Errorf("createInvite = %v, want errAccountDisabled", err)
	}
	if err := e.svc.deleteInvite(ctx, ws, inviteID); !errors.Is(err, errAccountDisabled) {
		t.Errorf("deleteInvite = %v, want errAccountDisabled", err)
	}
	if err := e.svc.setRole(ctx, ws, memberID, RoleAdmin); !errors.Is(err, errAccountDisabled) {
		t.Errorf("setRole = %v, want errAccountDisabled", err)
	}
	if err := e.svc.removeMember(ctx, ws, memberID); !errors.Is(err, errAccountDisabled) {
		t.Errorf("removeMember = %v, want errAccountDisabled", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"locale":"pt-BR"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(ctx, membershipKey{}, Membership{WorkspaceID: ws, Role: RoleAdmin}))
	rec := httptest.NewRecorder()
	e.svc.handleUpdateWorkspace(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), string(apierr.CodeAccountDisabled)) {
		t.Errorf("workspace settings: %d %s", rec.Code, rec.Body.String())
	}

	if n := e.count(`SELECT count(*) FROM invites WHERE email = 'new@example.com'`); n != 0 {
		t.Errorf("%d invites were made", n)
	}
	if n := e.count(`SELECT count(*) FROM invites WHERE id = ?`, inviteID); n != 1 {
		t.Error("the invite was withdrawn")
	}
	if n := e.count(`SELECT count(*) FROM memberships WHERE workspace_id = ? AND user_id = ? AND role = 'member'`, ws, memberID); n != 1 {
		t.Error("the member's role changed or the member was removed")
	}
	if n := e.count(`SELECT count(*) FROM workspaces WHERE id = ? AND locale = 'pt-BR'`, ws); n != 0 {
		t.Error("the workspace's locale changed")
	}
	if err := e.svc.setRole(audit.WithActor(context.Background(), e.userID(owner)), ws, memberID, RoleAdmin); err != nil {
		t.Fatalf("setRole by the owner: %v", err)
	}
}

func TestDisableEndsAnUnconfirmedEnrolment(t *testing.T) {
	tm := newTeam(t, Options{})
	expect(t, tm.member.do(http.MethodPost, "/api/me/two-factor/enrol", map[string]string{"password": otherPassword}), http.StatusOK)
	if n := tm.e.count(`SELECT count(*) FROM user_totp WHERE user_id = ? AND enabled_at IS NULL`, tm.memberID); n != 1 {
		t.Fatalf("%d enrolments before", n)
	}
	tm.disable()
	if n := tm.e.count(`SELECT count(*) FROM user_totp WHERE user_id = ?`, tm.memberID); n != 0 {
		t.Fatalf("%d enrolments after disabling", n)
	}
}

func TestDisableEndsHalfDoneLogins(t *testing.T) {
	tm := newTeam(t, Options{})
	e := tm.e
	tf := e.turnOn(tm.member, otherPassword)

	laptop := &client{e: e}
	rec := laptop.login("member@example.com", otherPassword)
	expect(t, rec, http.StatusOK)
	challenge := decode[struct {
		Challenge string `json:"challenge"`
	}](t, rec).Challenge
	if challenge == "" {
		t.Fatal("no challenge")
	}
	if n := e.count(`SELECT count(*) FROM login_challenges WHERE user_id = ?`, tm.memberID); n != 1 {
		t.Fatalf("%d challenges before", n)
	}
	tm.disable()
	if n := e.count(`SELECT count(*) FROM login_challenges WHERE user_id = ?`, tm.memberID); n != 0 {
		t.Errorf("%d challenges after disabling", n)
	}
	expect(t, laptop.do(http.MethodPost, "/api/login/two-factor", map[string]string{"challenge": challenge, "code": e.codeNow(tf.secret)}), http.StatusUnauthorized)
	expect(t, (&client{e: e}).login("member@example.com", otherPassword), http.StatusForbidden)
	if n := e.count(`SELECT count(*) FROM login_challenges WHERE user_id = ?`, tm.memberID); n != 0 {
		t.Errorf("a disabled account earned %d challenges", n)
	}

	tm.enable()
	e.nextStep()
	rec = (&client{e: e}).login("member@example.com", otherPassword)
	expect(t, rec, http.StatusOK)
	if got := decode[struct {
		Required bool `json:"two_factor_required"`
	}](t, rec); !got.Required {
		t.Fatal("two-factor login was lost by disabling")
	}
}
