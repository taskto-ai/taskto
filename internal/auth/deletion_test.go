package auth

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
)

// purgeLog is a DeletionScheduler that notes what it is asked.
type purgeLog struct {
	mu        sync.Mutex
	scheduled map[int64]time.Time
	cancelled []int64
}

func newPurgeLog() *purgeLog { return &purgeLog{scheduled: map[int64]time.Time{}} }

func (r *purgeLog) SchedulePurge(_ context.Context, _ *sql.Tx, userID, _ int64, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scheduled[userID] = at
	return nil
}

func (r *purgeLog) CancelPurge(_ context.Context, _ *sql.Tx, userID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.scheduled, userID)
	r.cancelled = append(r.cancelled, userID)
	return nil
}

func (tm *team) deletionPath() string { return "/api/me/deletion" }

func (tm *team) memberDeletionPath(ws, user int64) string {
	return fmt.Sprintf("/api/workspaces/%d/members/%d/deletion", ws, user)
}

func (tm *team) pendingPath() string {
	return fmt.Sprintf("/api/workspaces/%d/account-deletions", tm.ws)
}

func (tm *team) cancelPath(user int64) string {
	return fmt.Sprintf("/api/workspaces/%d/account-deletions/%d", tm.ws, user)
}

func newDeletionTeam(t *testing.T) (*team, *purgeLog) {
	t.Helper()
	tm := newTeam(t, Options{})
	rec := newPurgeLog()
	tm.e.svc.SetDeletionScheduler(rec)
	return tm, rec
}

func TestDeletingYourOwnAccountNeedsYourPasswordAndSwitchesItOffAtOnce(t *testing.T) {
	tm, sched := newDeletionTeam(t)
	e := tm.e

	status := decode[DeletionStatus](t, tm.member.do(http.MethodGet, tm.deletionPath(), nil))
	if status.GraceDays != 14 || status.Method != "password" || status.TwoFactor || len(status.Blockers) != 0 {
		t.Fatalf("status = %+v", status)
	}

	// Without the password, or with another one, nothing happens.
	rec := tm.member.do(http.MethodPost, tm.deletionPath(), map[string]string{})
	expect(t, rec, http.StatusBadRequest)
	rec = tm.member.do(http.MethodPost, tm.deletionPath(), map[string]string{"password": "not-the-password"})
	expect(t, rec, http.StatusForbidden)
	if got := errCode(t, rec); got != string(apierr.CodeWrongPassword) {
		t.Fatalf("code = %q", got)
	}
	expect(t, tm.member.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	if n := e.count(`SELECT count(*) FROM account_deletions`); n != 0 {
		t.Fatalf("a refused request left %d deletions", n)
	}

	before := e.now
	rec = tm.member.do(http.MethodPost, tm.deletionPath(), map[string]string{"password": otherPassword})
	expect(t, rec, http.StatusAccepted)
	reply := decode[DeletionReply](t, rec)
	if want := stamp(before.Add(14 * 24 * time.Hour)); reply.PurgeAt != want {
		t.Fatalf("purge_at = %s, want %s", reply.PurgeAt, want)
	}
	if at := sched.scheduled[tm.memberID]; !at.Equal(before.Add(14 * 24 * time.Hour)) {
		t.Fatalf("the purge was scheduled for %v", at)
	}

	// Switched off at once, as a disabled account is: every login and token ended.
	expect(t, tm.member.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	expect(t, tm.phone.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	if code := e.withToken(tm.token, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", tm.ws), nil); code != http.StatusUnauthorized {
		t.Errorf("the access token works: %d", code)
	}
	rec = e.client().do(http.MethodPost, "/api/login", map[string]string{"email": "member@example.com", "password": otherPassword})
	expect(t, rec, http.StatusForbidden)
	if n := e.count(`SELECT count(*) FROM users WHERE id = ? AND disabled_at IS NOT NULL`, tm.memberID); n != 1 {
		t.Errorf("the account isn't disabled")
	}
	// Nothing was deleted yet: that is the purge's, after the grace period.
	if n := e.count(`SELECT count(*) FROM memberships WHERE user_id = ?`, tm.memberID); n != 1 {
		t.Errorf("the membership went at once")
	}

	// The record says who and what, and the admin sees it in the members' list and the operator's list.
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action = ? AND actor_id = ? AND target_id = ? AND json_extract(details, '$.by_self') = 1`,
		audit.AccountDeletionRequested, tm.memberID, tm.memberID); n != 1 {
		t.Errorf("%d deletion events", n)
	}
	prec := tm.owner.do(http.MethodGet, tm.pendingPath(), nil)
	expect(t, prec, http.StatusOK)
	pending := decode[struct{ Deletions []PendingDeletion }](t, prec)
	if len(pending.Deletions) != 1 || pending.Deletions[0].User.ID != tm.memberID || !pending.Deletions[0].BySelf {
		t.Fatalf("pending = %+v", pending)
	}
	roster := decode[struct{ Members []Member }](t, tm.owner.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", tm.ws), nil))
	for _, m := range roster.Members {
		if (m.ID == tm.memberID) != (m.DeletionAt != nil) || (m.ID == tm.memberID && !m.Disabled) {
			t.Errorf("member %d: deletion_at %v, disabled %v", m.ID, m.DeletionAt, m.Disabled)
		}
	}
}

func TestDeletionWithTwoFactorNeedsTheCodeToo(t *testing.T) {
	tm, _ := newDeletionTeam(t)
	e := tm.e
	factor := e.turnOn(tm.member, otherPassword)
	if !decode[DeletionStatus](t, tm.member.do(http.MethodGet, tm.deletionPath(), nil)).TwoFactor {
		t.Fatal("status doesn't say two-factor is on")
	}

	// The password alone isn't enough, nor is a wrong code.
	rec := tm.member.do(http.MethodPost, tm.deletionPath(), map[string]string{"password": otherPassword})
	expect(t, rec, http.StatusForbidden)
	rec = tm.member.do(http.MethodPost, tm.deletionPath(), map[string]string{"password": otherPassword, "code": "000000"})
	if rec.Code < 400 {
		t.Fatalf("a wrong code was accepted: %d", rec.Code)
	}
	if n := e.count(`SELECT count(*) FROM account_deletions`); n != 0 {
		t.Fatal("deleted without the code")
	}
	rec = tm.member.do(http.MethodPost, tm.deletionPath(), map[string]string{"password": otherPassword, "code": e.codeNow(factor.secret)})
	expect(t, rec, http.StatusAccepted)
}

func TestAnAccountWithNoPasswordProvesItselfWithAFreshLogin(t *testing.T) {
	tm, _ := newDeletionTeam(t)
	e := tm.e
	// A person who signs in with a provider has no password.
	if _, err := e.db.Exec(`UPDATE users SET password_hash = '' WHERE id = ?`, tm.memberID); err != nil {
		t.Fatal(err)
	}
	status := decode[DeletionStatus](t, tm.member.do(http.MethodGet, tm.deletionPath(), nil))
	if status.Method != "fresh_login" || !status.Fresh {
		t.Fatalf("status = %+v", status)
	}

	// Their session is old: they sign in again first.
	e.advance(10 * time.Minute)
	if decode[DeletionStatus](t, tm.member.do(http.MethodGet, tm.deletionPath(), nil)).Fresh {
		t.Fatal("an old login counts as fresh")
	}
	rec := tm.member.do(http.MethodPost, tm.deletionPath(), map[string]string{})
	expect(t, rec, http.StatusForbidden)
	if got := errCode(t, rec); got != string(apierr.CodeDeletionReauth) {
		t.Fatalf("code = %q", got)
	}
	if n := e.count(`SELECT count(*) FROM account_deletions`); n != 0 {
		t.Fatal("deleted on an old login")
	}

	// A new session, made now: that proves it.
	if _, err := e.db.Exec(`UPDATE sessions SET created_at = ? WHERE user_id = ?`, stamp(e.now), tm.memberID); err != nil {
		t.Fatal(err)
	}
	expect(t, tm.member.do(http.MethodPost, tm.deletionPath(), map[string]string{}), http.StatusAccepted)
}

func TestTheLastWorkingAdminCantDeleteTheAccount(t *testing.T) {
	tm, _ := newDeletionTeam(t)
	e := tm.e

	// The owner is the only admin of the workspace the server's setup made: its operator.
	rec := tm.owner.do(http.MethodPost, tm.deletionPath(), map[string]string{"password": ownerPassword})
	expect(t, rec, http.StatusConflict)
	if got := errCode(t, rec); got != string(apierr.CodeDeletionLastOperator) {
		t.Fatalf("code = %q", got)
	}
	status := decode[DeletionStatus](t, tm.owner.do(http.MethodGet, tm.deletionPath(), nil))
	if len(status.Blockers) != 1 || !status.Blockers[0].Operator || status.Blockers[0].WorkspaceID != tm.ws {
		t.Fatalf("blockers = %+v", status.Blockers)
	}

	// A member who is the only admin of a workspace of their own is blocked there too.
	other := e.addWorkspace(tm.memberID, "Side project")
	rec = tm.member.do(http.MethodPost, tm.deletionPath(), map[string]string{"password": otherPassword})
	expect(t, rec, http.StatusConflict)
	if got := errCode(t, rec); got != string(apierr.CodeDeletionLastAdmin) {
		t.Fatalf("code = %q", got)
	}
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, other, tm.ownerID); err != nil {
		t.Fatal(err)
	}
	expect(t, tm.member.do(http.MethodPost, tm.deletionPath(), map[string]string{"password": otherPassword}), http.StatusAccepted)

	// An admin who is disabled doesn't count as a successor: the owner is the only one in use.
	if _, err := e.db.Exec(`UPDATE memberships SET role = 'admin' WHERE workspace_id = ? AND user_id = ?`, tm.ws, tm.memberID); err != nil {
		t.Fatal(err)
	}
	rec = tm.owner.do(http.MethodPost, tm.deletionPath(), map[string]string{"password": ownerPassword})
	expect(t, rec, http.StatusConflict)
	if n := e.count(`SELECT count(*) FROM account_deletions WHERE user_id = ?`, tm.ownerID); n != 0 {
		t.Fatal("the last admin's deletion was recorded")
	}
}

func TestAnAdminDeletesAMemberUnderTheResetLinkRule(t *testing.T) {
	tm, sched := newDeletionTeam(t)
	e := tm.e

	// Not a member's own to do to others, nor to oneself through this route.
	expect(t, tm.member.do(http.MethodPost, tm.memberDeletionPath(tm.ws, tm.ownerID), nil), http.StatusForbidden)
	rec := tm.owner.do(http.MethodPost, tm.memberDeletionPath(tm.ws, tm.ownerID), nil)
	expect(t, rec, http.StatusBadRequest)
	if got := errCode(t, rec); got != string(apierr.CodeDeletionOwn) {
		t.Fatalf("code = %q", got)
	}
	// A person that isn't in the workspace is not found, whoever they are.
	stranger, _ := e.join(e.invite(tm.owner, e.addWorkspace(tm.ownerID, "Elsewhere"), "stranger@example.com", RoleMember), "Stranger")
	expect(t, tm.owner.do(http.MethodPost, tm.memberDeletionPath(tm.ws, e.userID(stranger)), nil), http.StatusNotFound)

	// A member who is also in a workspace the admin doesn't administer: refused, as for a reset link.
	spare := e.addWorkspace(tm.memberID, "Member's own")
	rec = tm.owner.do(http.MethodPost, tm.memberDeletionPath(tm.ws, tm.memberID), nil)
	expect(t, rec, http.StatusForbidden)
	if got := errCode(t, rec); got != string(apierr.CodeDeletionElsewhere) {
		t.Fatalf("code = %q", got)
	}
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, spare, tm.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, spare, tm.ownerID+1000); err == nil {
		t.Fatal("fixture: a membership of a user that doesn't exist")
	}

	// Their own workspace has the member as its only admin, and the owner too now: fine.
	rec = tm.owner.do(http.MethodPost, tm.memberDeletionPath(tm.ws, tm.memberID), nil)
	expect(t, rec, http.StatusAccepted)
	if _, ok := sched.scheduled[tm.memberID]; !ok {
		t.Fatal("no purge was scheduled")
	}
	expect(t, tm.member.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action = ? AND actor_id = ? AND target_id = ? AND json_extract(details, '$.by_self') = 0`,
		audit.AccountDeletionRequested, tm.ownerID, tm.memberID); n < 1 {
		t.Errorf("no event says the admin deleted the account")
	}

	// It is waiting already: again is refused, and so is disabling or enabling it.
	rec = tm.owner.do(http.MethodPost, tm.memberDeletionPath(tm.ws, tm.memberID), nil)
	expect(t, rec, http.StatusConflict)
	if got := errCode(t, rec); got != string(apierr.CodeDeletionPending) {
		t.Fatalf("code = %q", got)
	}
	rec = tm.owner.do(http.MethodPost, e.disablePath(tm.ws, tm.memberID, "enable"), nil)
	expect(t, rec, http.StatusConflict)
	if got := errCode(t, rec); got != string(apierr.CodeDeletionPending) {
		t.Fatalf("enable: code = %q", got)
	}
}

func TestOnlyAnOperatorCancelsADeletionAndTheAccountComesBack(t *testing.T) {
	tm, sched := newDeletionTeam(t)
	e := tm.e
	expect(t, tm.member.do(http.MethodPost, tm.deletionPath(), map[string]string{"password": otherPassword}), http.StatusAccepted)

	// A member, and an admin of another workspace who isn't the operator, can neither see nor cancel.
	expect(t, tm.phone.do(http.MethodDelete, tm.cancelPath(tm.memberID), nil), http.StatusUnauthorized)
	side := e.addWorkspace(tm.memberID, "Side")
	_ = side
	adminElsewhere, _ := e.join(e.invite(tm.owner, tm.ws, "second@example.com", RoleMember), "Second")
	secondID := e.userID(adminElsewhere)
	other := e.addWorkspace(secondID, "Second's workspace")
	expect(t, adminElsewhere.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/account-deletions", other), nil), http.StatusForbidden)
	expect(t, adminElsewhere.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/account-deletions/%d", other, tm.memberID), nil), http.StatusForbidden)
	if n := e.count(`SELECT count(*) FROM account_deletions`); n != 1 {
		t.Fatal("a refused cancel changed the deletion")
	}
	// A token can't, and a deletion that isn't there is not found.
	rec := tm.owner.do(http.MethodDelete, tm.cancelPath(tm.ownerID), nil)
	expect(t, rec, http.StatusNotFound)
	if got := errCode(t, rec); got != string(apierr.CodeDeletionNotPending) {
		t.Fatalf("code = %q", got)
	}

	// The operator cancels: the account is on again, with its old login, and its purge is gone.
	expect(t, tm.owner.do(http.MethodDelete, tm.cancelPath(tm.memberID), nil), http.StatusNoContent)
	if _, ok := sched.scheduled[tm.memberID]; ok {
		t.Fatal("the purge is still scheduled")
	}
	if n := e.count(`SELECT count(*) FROM account_deletions`); n != 0 {
		t.Fatal("the deletion is still recorded")
	}
	if _, code := e.login("member@example.com", otherPassword); code != http.StatusOK {
		t.Fatalf("login after the cancel: %d", code)
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action = ? AND target_id = ?`, audit.AccountDeletionCancelled, tm.memberID); n < 1 {
		t.Errorf("%d cancel events", n)
	}
}

func TestACancelledDeletionLeavesADisabledAccountDisabled(t *testing.T) {
	tm, _ := newDeletionTeam(t)
	tm.disable()
	expect(t, tm.owner.do(http.MethodPost, tm.memberDeletionPath(tm.ws, tm.memberID), nil), http.StatusAccepted)
	expect(t, tm.owner.do(http.MethodDelete, tm.cancelPath(tm.memberID), nil), http.StatusNoContent)
	if n := tm.e.count(`SELECT count(*) FROM users WHERE id = ? AND disabled_at IS NOT NULL`, tm.memberID); n != 1 {
		t.Fatal("cancelling the deletion enabled an account an admin had disabled")
	}
}

func TestADeletionCantBeCancelledOnceThePurgeHasBegun(t *testing.T) {
	tm, _ := newDeletionTeam(t)
	expect(t, tm.member.do(http.MethodPost, tm.deletionPath(), map[string]string{"password": otherPassword}), http.StatusAccepted)
	if _, err := tm.e.db.Exec(`UPDATE account_deletions SET purge_started_at = ?`, stamp(tm.e.now)); err != nil {
		t.Fatal(err)
	}
	rec := tm.owner.do(http.MethodDelete, tm.cancelPath(tm.memberID), nil)
	expect(t, rec, http.StatusConflict)
	if got := errCode(t, rec); got != string(apierr.CodeDeletionPurging) {
		t.Fatalf("code = %q", got)
	}
	prec := tm.owner.do(http.MethodGet, tm.pendingPath(), nil)
	expect(t, prec, http.StatusOK)
	pending := decode[struct{ Deletions []PendingDeletion }](t, prec)
	if len(pending.Deletions) != 1 || !pending.Deletions[0].Purging {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestDeletionRoutesTakeNoAccessTokens(t *testing.T) {
	tm, _ := newDeletionTeam(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, tm.deletionPath()},
		{http.MethodPost, tm.deletionPath()},
		{http.MethodPost, tm.memberDeletionPath(tm.ws, tm.memberID)},
		{http.MethodGet, tm.pendingPath()},
		{http.MethodDelete, tm.cancelPath(tm.memberID)},
	} {
		if code := tm.e.withToken(tm.token, c.method, c.path, map[string]string{"password": otherPassword}); code < 400 || code == http.StatusInternalServerError {
			t.Errorf("%s %s with an access token: %d", c.method, c.path, code)
		}
	}
	if n := tm.e.count(`SELECT count(*) FROM account_deletions`); n != 0 {
		t.Fatal("a token deleted an account")
	}
}

func TestRemovingAMemberWhoseAccountIsWaitingKeepsItsPurge(t *testing.T) {
	tm, sched := newDeletionTeam(t)
	e := tm.e
	second := e.addWorkspace(tm.memberID, "Second")
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, second, tm.ownerID); err != nil {
		t.Fatal(err)
	}
	expect(t, tm.owner.do(http.MethodPost, tm.memberDeletionPath(tm.ws, tm.memberID), nil), http.StatusAccepted)
	delete(sched.scheduled, tm.memberID) // as the scheduler's own trigger does when the membership goes
	expect(t, tm.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", tm.ws, tm.memberID), nil), http.StatusNoContent)
	if _, ok := sched.scheduled[tm.memberID]; !ok {
		t.Fatal("the purge was not filed again when the member was removed")
	}
}
