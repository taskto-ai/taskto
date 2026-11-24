package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/audit/audittest"
)

// events returns the audit events of action, oldest first.
func (e *env) events(action string) []audit.Row {
	e.t.Helper()
	rows, err := audit.List(context.Background(), e.db, audit.Filter{Action: action, Limit: 1000})
	if err != nil {
		e.t.Fatal(err)
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows
}

// one returns the only event of action, failing if there isn't exactly one.
func (e *env) one(action string) audit.Row {
	e.t.Helper()
	rows := e.events(action)
	if len(rows) != 1 {
		e.t.Fatalf("%s: %d events, want 1: %+v", action, len(rows), rows)
	}
	return rows[0]
}

func wantID(t *testing.T, what string, got *int64, want int64) {
	t.Helper()
	switch {
	case want == 0 && got != nil:
		t.Errorf("%s = %d, want none", what, *got)
	case want != 0 && (got == nil || *got != want):
		t.Errorf("%s = %v, want %d", what, got, want)
	}
}

// want checks who did it, in which workspace, to what.
func wantEvent(t *testing.T, r audit.Row, workspace, actor int64, targetType string, target int64) {
	t.Helper()
	wantID(t, r.Action+" workspace", r.WorkspaceID, workspace)
	wantID(t, r.Action+" actor", r.ActorID, actor)
	wantID(t, r.Action+" target", r.TargetID, target)
	if r.TargetType != targetType {
		t.Errorf("%s target type = %q, want %q", r.Action, r.TargetType, targetType)
	}
	if r.ClientAddr == nil || *r.ClientAddr != "192.0.2.1" {
		t.Errorf("%s client address = %v, want 192.0.2.1", r.Action, r.ClientAddr)
	}
	if r.At == "" {
		t.Errorf("%s has no time", r.Action)
	}
}

func TestAuditSetupLoginLogout(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := e.userID(owner)

	wantEvent(t, e.one(audit.SetupCompleted), ws, ownerID, audit.TargetWorkspace, ws)

	// A login.
	c, code := e.login("owner@example.com", ownerPassword)
	if code != http.StatusOK {
		t.Fatalf("login = %d", code)
	}
	ok := e.one(audit.LoginSucceeded)
	wantEvent(t, ok, 0, ownerID, audit.TargetUser, ownerID)
	if ok.Details["method"] != "password" {
		t.Errorf("details = %v", ok.Details)
	}

	// Failures: a wrong password for a known account is the account's; an unknown address has no
	// actor, and the address typed is nowhere in the log.
	if _, code := e.login("owner@example.com", "not-the-password"); code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", code)
	}
	if _, code := e.login("typo-of-a-password@example.com", "whatever-password"); code != http.StatusUnauthorized {
		t.Fatalf("unknown account = %d", code)
	}
	failed := e.events(audit.LoginFailed)
	if len(failed) != 2 {
		t.Fatalf("login_failed = %d events, want 2", len(failed))
	}
	wantEvent(t, failed[0], 0, ownerID, audit.TargetUser, ownerID)
	if failed[0].Details["reason"] != "wrong_password" {
		t.Errorf("details = %v", failed[0].Details)
	}
	wantEvent(t, failed[1], 0, 0, "", 0)
	if failed[1].Details["reason"] != "unknown_account" {
		t.Errorf("details = %v", failed[1].Details)
	}

	// Logout.
	expect(t, c.do(http.MethodPost, "/api/logout", nil), http.StatusNoContent)
	wantEvent(t, e.one(audit.Logout), 0, ownerID, audit.TargetUser, ownerID)
	// Logging out again, with no session, is no event.
	expect(t, c.do(http.MethodPost, "/api/logout", nil), http.StatusNoContent)
	if n := len(e.events(audit.Logout)); n != 1 {
		t.Fatalf("logout events = %d, want 1", n)
	}
}

func TestAuditRefusedLoginsFillNoLog(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	for i := 0; i < accountAndIPAttempts+5; i++ {
		e.login("owner@example.com", "not-the-password")
	}
	// The limiter turns the later attempts away before they are weighed: they leave no event.
	if n := len(e.events(audit.LoginFailed)); n != accountAndIPAttempts {
		t.Fatalf("login_failed = %d events, want %d", n, accountAndIPAttempts)
	}
}

func TestAuditPasswordChangeAndReset(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := e.userID(owner)
	member, mi := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	memberID := mi.User.ID

	// Changing one's own password: a wrong current one, then a right one.
	expect(t, member.do(http.MethodPost, "/api/me/password",
		map[string]string{"current_password": "wrong-current", "new_password": newPassword}), http.StatusForbidden)
	wantEvent(t, e.one(audit.PasswordChangeFailed), 0, memberID, audit.TargetUser, memberID)
	expect(t, member.do(http.MethodPost, "/api/me/password",
		map[string]string{"current_password": otherPassword, "new_password": newPassword}), http.StatusNoContent)
	wantEvent(t, e.one(audit.PasswordChanged), 0, memberID, audit.TargetUser, memberID)

	// An admin makes a reset link for the member: the event is the workspace's, the admin's.
	token := e.reset(owner, ws, memberID)
	wantEvent(t, e.one(audit.PasswordResetCreated), ws, ownerID, audit.TargetUser, memberID)

	// The member uses it, anonymously: the actor is the user whose password it set.
	anon := e.client()
	expect(t, anon.do(http.MethodPost, "/api/password-reset", map[string]string{"token": token, "password": "fake-third-password"}), http.StatusOK)
	wantEvent(t, e.one(audit.PasswordResetUsed), 0, memberID, audit.TargetUser, memberID)

	// A link that no longer works leaves no event.
	expect(t, anon.do(http.MethodPost, "/api/password-reset", map[string]string{"token": token, "password": "fake-fourth-password"}), http.StatusNotFound)
	if n := len(e.events(audit.PasswordResetUsed)); n != 1 {
		t.Fatalf("password_reset_used = %d events, want 1", n)
	}
}

func TestAuditInvitesMembersAndWorkspace(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := e.userID(owner)

	// Create, then accept as a new person.
	token := e.invite(owner, ws, "member@example.com", RoleMember)
	created := e.one(audit.InviteCreated)
	wantEvent(t, created, ws, ownerID, audit.TargetInvite, *created.TargetID)
	if created.Details["email"] != "member@example.com" || created.Details["role"] != "member" {
		t.Errorf("details = %v", created.Details)
	}
	_, mi := e.join(token, "Member")
	memberID := mi.User.ID
	accepted := e.one(audit.InviteAccepted)
	wantEvent(t, accepted, ws, memberID, audit.TargetInvite, *created.TargetID)
	if accepted.Details["role"] != "member" || accepted.Details["already_member"] != false {
		t.Errorf("details = %v", accepted.Details)
	}

	// Withdraw one.
	rec := owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/invites", ws), map[string]string{"email": "late@example.com", "role": "admin"})
	expect(t, rec, http.StatusCreated)
	lateID := decode[Invite](t, rec).ID
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/invites/%d", ws, lateID), nil), http.StatusNoContent)
	revoked := e.one(audit.InviteRevoked)
	wantEvent(t, revoked, ws, ownerID, audit.TargetInvite, lateID)
	if revoked.Details["email"] != "late@example.com" || revoked.Details["role"] != "admin" {
		t.Errorf("details = %v", revoked.Details)
	}
	// Withdrawing it again is an error, and no event.
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/invites/%d", ws, lateID), nil), http.StatusNotFound)
	if n := len(e.events(audit.InviteRevoked)); n != 1 {
		t.Fatalf("invite_revoked = %d events, want 1", n)
	}

	// Accepting as a logged-in existing user.
	second := e.addWorkspace(ownerID, "Second")
	inv := directInvite(t, e, second, "member@example.com")
	member, _ := e.login("member@example.com", otherPassword)
	expect(t, member.do(http.MethodPost, "/api/invites/accept", map[string]string{"token": inv}), http.StatusOK)
	var existing audit.Row
	for _, r := range e.events(audit.InviteAccepted) {
		if r.WorkspaceID != nil && *r.WorkspaceID == second {
			existing = r
		}
	}
	wantEvent(t, existing, second, memberID, audit.TargetInvite, *existing.TargetID)

	// A role change and a removal.
	expect(t, owner.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, memberID), map[string]string{"role": "admin"}), http.StatusNoContent)
	role := e.one(audit.MemberRoleChanged)
	wantEvent(t, role, ws, ownerID, audit.TargetUser, memberID)
	if role.Details["from"] != "member" || role.Details["to"] != "admin" {
		t.Errorf("details = %v", role.Details)
	}
	// The same role again changes nothing, so it records nothing.
	expect(t, owner.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, memberID), map[string]string{"role": "admin"}), http.StatusNoContent)
	if n := len(e.events(audit.MemberRoleChanged)); n != 1 {
		t.Fatalf("member_role_changed = %d events, want 1", n)
	}
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, memberID), nil), http.StatusNoContent)
	removed := e.one(audit.MemberRemoved)
	wantEvent(t, removed, ws, ownerID, audit.TargetUser, memberID)
	if removed.Details["role"] != "admin" {
		t.Errorf("details = %v", removed.Details)
	}

	// The last admin can't leave: the refusal records nothing.
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", ws, ownerID), nil), http.StatusConflict)
	if n := len(e.events(audit.MemberRemoved)); n != 1 {
		t.Fatalf("member_removed = %d events, want 1", n)
	}

	// A workspace setting.
	expect(t, owner.do(http.MethodPatch, fmt.Sprintf("/api/workspaces/%d", ws), map[string]string{"locale": "pt-BR"}), http.StatusOK)
	upd := e.one(audit.WorkspaceUpdated)
	wantEvent(t, upd, ws, ownerID, audit.TargetWorkspace, ws)
	if upd.Details["locale"] != "pt-BR" {
		t.Errorf("details = %v", upd.Details)
	}
}

// directInvite makes an invite of workspace to email directly, for a workspace no client of the
// test administers, and returns its token.
func directInvite(t *testing.T, e *env, workspace int64, email string) string {
	t.Helper()
	token, hash := newToken()
	if _, err := e.db.Exec(`INSERT INTO invites (workspace_id, token_hash, email, role, created_by, expires_at)
		VALUES (?, ?, ?, 'member', (SELECT id FROM users ORDER BY id LIMIT 1), ?)`, workspace, hash, email, stamp(e.svc.now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	return token
}

func TestAuditAccessTokens(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := e.userID(owner)

	rec := owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/tokens", ws), map[string]string{"name": "laptop"})
	expect(t, rec, http.StatusCreated)
	id := decode[Token](t, rec).ID
	created := e.one(audit.AccessTokenCreated)
	wantEvent(t, created, ws, ownerID, audit.TargetAccessToken, id)
	if created.Details["name"] != "laptop" || created.Details["expires"] != false {
		t.Errorf("details = %v", created.Details)
	}

	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/tokens/%d", ws, id), nil), http.StatusNoContent)
	wantEvent(t, e.one(audit.AccessTokenRevoked), ws, ownerID, audit.TargetAccessToken, id)
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/tokens/%d", ws, id), nil), http.StatusNotFound)
	if n := len(e.events(audit.AccessTokenRevoked)); n != 1 {
		t.Fatalf("access_token_revoked = %d events, want 1", n)
	}
}

func TestAuditSSO(t *testing.T) {
	s := newSSOEnv(t)
	owner, ws := s.setup()
	ownerID := s.userID(owner)

	// An existing account is linked by its verified address.
	b := s.browser()
	rec := s.signIn(b, "", verified("owner@example.com"))
	s.wantLogin(b, rec, "/", "owner@example.com")
	linked := s.one(audit.SSOIdentityLinked)
	wantEvent(t, linked, 0, ownerID, audit.TargetUser, ownerID)
	if linked.Details["provider"] != "fake" || linked.Details["created_account"] != false {
		t.Errorf("details = %v", linked.Details)
	}
	wantEvent(t, s.one(audit.SSOLoginSucceeded), 0, ownerID, audit.TargetUser, ownerID)

	// A second login links nothing, and is a login.
	s.signIn(s.browser(), "", verified("owner@example.com"))
	if n := len(s.events(audit.SSOIdentityLinked)); n != 1 {
		t.Fatalf("sso_identity_linked = %d events, want 1", n)
	}
	if n := len(s.events(audit.SSOLoginSucceeded)); n != 2 {
		t.Fatalf("sso_login_succeeded = %d events, want 2", n)
	}

	// An invited newcomer: the account is made, linked and joined to the workspace.
	s.invite(owner, ws, "new@example.com", RoleMember)
	rec = s.signIn(s.browser(), "/invite", map[string]any{"email": "new@example.com", "email_verified": true, "sub": "sub-2"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("invited sign-in = %d", rec.Code)
	}
	var newID int64
	if err := s.db.QueryRow(`SELECT id FROM users WHERE email = 'new@example.com'`).Scan(&newID); err != nil {
		t.Fatal(err)
	}
	var created audit.Row
	for _, r := range s.events(audit.SSOIdentityLinked) {
		if *r.ActorID == newID {
			created = r
		}
	}
	if created.Details["created_account"] != true {
		t.Errorf("details = %v", created.Details)
	}
	var joined audit.Row
	for _, r := range s.events(audit.InviteAccepted) {
		joined = r
	}
	wantID(t, "invite_accepted actor", joined.ActorID, newID)

	// A refusal: a person with no account and no invite.
	b3 := s.browser()
	s.wantFailure(s.signIn(b3, "", map[string]any{"email": "stranger@example.com", "email_verified": true, "sub": "sub-9"}), "sso_no_account")
	failed := s.one(audit.SSOLoginFailed)
	wantEvent(t, failed, 0, 0, "", 0)
	if failed.Details["provider"] != "fake" || failed.Details["reason"] != "sso_no_account" {
		t.Errorf("details = %v", failed.Details)
	}
	if strings.Contains(fmt.Sprint(failed.Details), "stranger") {
		t.Errorf("the refused address is in the log: %v", failed.Details)
	}
}

// TestAuditHoldsNoSecret runs the account flows with known secret values and checks that none
// of them is in any column of any audit row.
func TestAuditHoldsNoSecret(t *testing.T) {
	const wrongPassword = "fake-wrong-password-9f2c"
	e := newEnv(t, Options{})
	owner, ws := e.setup()

	secrets := map[string]string{
		"setup token":    testSetupToken,
		"owner password": ownerPassword,
		"other password": otherPassword,
		"new password":   newPassword,
		"wrong password": wrongPassword,
	}
	secrets["session cookie"] = owner.cookie.Value

	inviteToken := e.invite(owner, ws, "member@example.com", RoleMember)
	secrets["invite token"] = inviteToken
	member, mi := e.join(inviteToken, "Member")
	secrets["member session cookie"] = member.cookie.Value

	secrets["access token"] = e.accessToken(owner, ws)
	secrets["reset token"] = e.reset(owner, ws, mi.User.ID)
	expect(t, member.do(http.MethodPost, "/api/me/password",
		map[string]string{"current_password": wrongPassword, "new_password": newPassword}), http.StatusForbidden)
	e.login("owner@example.com", wrongPassword)
	e.login("nobody@example.com", wrongPassword)
	expect(t, e.client().do(http.MethodPost, "/api/password-reset",
		map[string]string{"token": secrets["reset token"], "password": newPassword}), http.StatusOK)

	// Hashes of them count too: what the database keeps for a token isn't a thing to copy.
	for name, value := range map[string]string{"invite": inviteToken, "reset": secrets["reset token"], "access": secrets["access token"]} {
		secrets[name+" token hash"] = hashToken(value)
	}
	audittest.NoSecrets(t, e.db, secrets)

	// The log isn't empty: the check above looked at something.
	rows, err := audit.List(context.Background(), e.db, audit.Filter{Limit: 1000})
	if err != nil || len(rows) < 9 {
		t.Fatalf("%d audit events, %v", len(rows), err)
	}
}

func TestAuditCarriesTheClientBehindATrustedProxy(t *testing.T) {
	e := newEnv(t, Options{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}})
	e.setup()
	c := e.client()
	c.xff = "198.51.100.7"
	expect(t, c.do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": ownerPassword}), http.StatusOK)
	rows := e.events(audit.LoginSucceeded)
	if len(rows) != 1 || rows[0].ClientAddr == nil || *rows[0].ClientAddr != "198.51.100.7" {
		t.Fatalf("login events = %+v, want the forwarded client's address", rows)
	}
}

func TestAuditDetailsAreNotTheCallers(t *testing.T) {
	// The query string of an SSO callback, with its code, is never part of an event.
	s := newSSOEnv(t)
	s.setup()
	b := s.browser()
	q := s.start(b, "")
	rec := b.do(http.MethodGet, "/api/sso/fake/callback?state="+url.QueryEscape(q.Get("state"))+"&error=access_denied&error_description=secret-words", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback = %d", rec.Code)
	}
	failed := s.one(audit.SSOLoginFailed)
	if failed.Details["reason"] != "provider_error" || strings.Contains(fmt.Sprint(failed.Details), "secret-words") {
		t.Errorf("details = %v", failed.Details)
	}
}
