package auth

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/audit/audittest"
)

// Two-factor login's changes are audited, in the transaction of the change; a refused request
// records none; and no secret, code or challenge is in the log.
func TestAuditTwoFactor(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	member, memberMe := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	ownerID := e.userID(owner)
	memberID := memberMe.User.ID

	// Starting, or confirming with a wrong code, turns nothing on: no event.
	got, secret := e.enrol(owner, ownerPassword)
	if status, _ := e.confirmStatus(owner, got.Enrolment, wrongCode); status != http.StatusForbidden {
		t.Fatalf("wrong code: %d", status)
	}
	if n := len(e.events(audit.TwoFactorEnabled)); n != 0 {
		t.Fatalf("two_factor_enabled = %d events before a right code", n)
	}
	if status, _ := e.confirmStatus(owner, got.Enrolment, e.codeNow(secret)); status != http.StatusOK {
		t.Fatalf("confirm: %d", status)
	}
	e.nextStep()
	wantEvent(t, e.one(audit.TwoFactorEnabled), 0, ownerID, audit.TargetUser, ownerID)

	// A password login with a session is an event; with a challenge it is not, until the code.
	before := len(e.events(audit.LoginSucceeded))
	c := e.client()
	ch := c.challenge("owner@example.com", ownerPassword)
	if n := len(e.events(audit.LoginSucceeded)); n != before {
		t.Fatalf("a challenge recorded a login: %d events, had %d", n, before)
	}
	expect(t, c.second(ch, wrongCode), http.StatusUnauthorized)
	failed := e.events(audit.LoginFailed)
	last := failed[len(failed)-1]
	wantEvent(t, last, 0, 0, audit.TargetUser, ownerID)
	if last.Details["reason"] != "wrong_code" || last.Details["method"] != "password" {
		t.Errorf("details = %v", last.Details)
	}
	expect(t, c.second(ch, e.codeNow(secret)), http.StatusOK)
	rows := e.events(audit.LoginSucceeded)
	if len(rows) != before+1 {
		t.Fatalf("login_succeeded = %d events, want %d", len(rows), before+1)
	}
	wantEvent(t, rows[len(rows)-1], 0, ownerID, audit.TargetUser, ownerID)

	// New recovery codes, and turning it off: with a wrong code, nothing is recorded.
	e.nextStep()
	rec := owner.do(http.MethodPost, "/api/me/two-factor/recovery-codes", map[string]string{"password": ownerPassword, "code": wrongCode})
	expect(t, rec, http.StatusForbidden)
	if n := len(e.events(audit.TwoFactorRecoveryCodesReplaced)); n != 0 {
		t.Fatalf("two_factor_recovery_codes_replaced = %d events after a refusal", n)
	}
	rec = owner.do(http.MethodPost, "/api/me/two-factor/recovery-codes", map[string]string{"password": ownerPassword, "code": e.codeNow(secret)})
	expect(t, rec, http.StatusOK)
	newCodes := decode[struct {
		Codes []string `json:"recovery_codes"`
	}](t, rec).Codes
	wantEvent(t, e.one(audit.TwoFactorRecoveryCodesReplaced), 0, ownerID, audit.TargetUser, ownerID)
	rec = owner.do(http.MethodPost, "/api/me/two-factor/disable", map[string]string{"password": ownerPassword, "code": newCodes[0]})
	expect(t, rec, http.StatusNoContent)
	wantEvent(t, e.one(audit.TwoFactorDisabled), 0, ownerID, audit.TargetUser, ownerID)

	// An admin clears a member's: the workspace's event, the admin the actor.
	memberTwo := e.turnOn(member, otherPassword)
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d/two-factor", ws, memberID), nil), http.StatusNoContent)
	wantEvent(t, e.one(audit.TwoFactorCleared), ws, ownerID, audit.TargetUser, memberID)
	if n := len(e.events(audit.TwoFactorCleared)); n != 1 {
		t.Fatalf("two_factor_cleared = %d", n)
	}
	// Clearing what isn't on is refused, and no event.
	expect(t, owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d/two-factor", ws, memberID), nil), http.StatusConflict)
	if n := len(e.events(audit.TwoFactorCleared)); n != 1 {
		t.Fatalf("two_factor_cleared = %d after a refusal", n)
	}

	secrets := map[string]string{
		"challenge":        ch,
		"challenge hash":   hashToken(ch),
		"owner secret":     string(secret),
		"owner secret b32": secretEncoding.EncodeToString(secret),
		"member secret":    secretEncoding.EncodeToString(memberTwo.secret),
		"enrolment":        got.Enrolment,
		"enrolment hash":   hashToken(got.Enrolment),
	}
	for i, code := range append(newCodes, memberTwo.codes...) {
		secrets[fmt.Sprintf("recovery code %d", i)] = code
		secrets[fmt.Sprintf("recovery code %d plain", i)] = strings.ReplaceAll(code, "-", "")
	}
	audittest.NoSecrets(t, e.db, secrets)
}
