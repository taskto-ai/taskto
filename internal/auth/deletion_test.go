package auth

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
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
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action = ? AND actor_id = ? AND target_id = ?`,
		audit.AccountDeletionRequested, tm.memberID, tm.memberID); n != 1 {
		t.Errorf("%d deletion events", n)
	}
	prec := tm.owner.do(http.MethodGet, tm.pendingPath(), nil)
	expect(t, prec, http.StatusOK)
	pending := decode[struct{ Deletions []PendingDeletion }](t, prec)
	if len(pending.Deletions) != 1 || pending.Deletions[0].User.ID != tm.memberID {
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

func TestNobodyButThePersonDeletesAnAccount(t *testing.T) {
	tm, sched := newDeletionTeam(t)
	e := tm.e

	// The route an admin deleted a member's account by is gone: an admin, who administers every
	// workspace the member belongs to, gets neither an acceptance nor a refusal of a kind that
	// would say the route exists.
	for _, c := range []struct {
		who  *client
		user int64
	}{{tm.owner, tm.memberID}, {tm.owner, tm.ownerID}, {tm.member, tm.ownerID}, {tm.member, tm.memberID}} {
		rec := c.who.do(http.MethodPost, tm.memberDeletionPath(tm.ws, c.user), map[string]string{})
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d, want 404 or 405", tm.memberDeletionPath(tm.ws, c.user), rec.Code)
		}
	}
	if n := e.count(`SELECT count(*) FROM account_deletions`); n != 0 {
		t.Fatalf("%d deletions were recorded by a route that is gone", n)
	}
	if n := e.count(`SELECT count(*) FROM users WHERE disabled_at IS NOT NULL`); n != 0 {
		t.Fatal("an account was switched off")
	}
	if len(sched.scheduled) != 0 {
		t.Fatal("a purge was scheduled")
	}
	expect(t, tm.member.do(http.MethodGet, "/api/me", nil), http.StatusOK)

	// What an admin may still do with a member is remove them from the workspace, which leaves
	// their account, and what they own, alone.
	expect(t, tm.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", tm.ws, tm.memberID), nil), http.StatusNoContent)
	if n := e.count(`SELECT count(*) FROM users WHERE id = ?`, tm.memberID); n != 1 {
		t.Fatal("removing a member deleted their account")
	}
	if n := e.count(`SELECT count(*) FROM account_deletions`); n != 0 {
		t.Fatal("removing a member recorded a deletion")
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
	// The member can't sign in to ask, so the deletion is recorded as the person's request would.
	if err := tm.e.svc.write(context.Background(), func(tx *sql.Tx) error {
		_, err := tm.e.svc.requestDeletion(context.Background(), tx, tm.memberID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
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

func TestRemovingAMemberWhoseAccountIsWaitingLeavesItsPurge(t *testing.T) {
	tm, sched := newDeletionTeam(t)
	e := tm.e
	second := e.addWorkspace(tm.memberID, "Second")
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, second, tm.ownerID); err != nil {
		t.Fatal(err)
	}
	expect(t, tm.member.do(http.MethodPost, tm.deletionPath(), map[string]string{"password": otherPassword}), http.StatusAccepted)
	expect(t, tm.owner.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d", tm.ws, tm.memberID), nil), http.StatusNoContent)
	if _, ok := sched.scheduled[tm.memberID]; !ok {
		t.Fatal("the purge was dropped when the member was removed")
	}
	if len(sched.cancelled) != 0 {
		t.Fatal("removing the member cancelled their purge")
	}
}

// SQLite gives the next account the largest id plus one, so the newest account's id would go to
// whoever registers after it is purged. What is kept by id (a browser's drafts and offline records,
// the mentions in comments) would then be theirs. The id is never given again.
func TestAPurgedNewestAccountsIDIsNotGivenToTheNextToRegister(t *testing.T) {
	tm, _ := newDeletionTeam(t)
	e := tm.e
	last, _ := e.join(e.invite(tm.owner, tm.ws, "last@example.com", RoleMember), "Last")
	lastID := e.userID(last)
	var newest int64
	if err := e.db.QueryRow(`SELECT max(id) FROM users`).Scan(&newest); err != nil || newest != lastID {
		t.Fatalf("fixture: the newest account is %d, %v; want %d", newest, err, lastID)
	}

	// What the purge does, last: the account's private spaces and then its row go.
	for _, q := range []string{`DELETE FROM spaces WHERE owner_id = ?`, `DELETE FROM users WHERE id = ?`} {
		if _, err := e.db.Exec(q, lastID); err != nil {
			t.Fatal(err)
		}
	}
	again, _ := e.join(e.invite(tm.owner, tm.ws, "last@example.com", RoleMember), "Last Again")
	if got := e.userID(again); got <= lastID {
		t.Fatalf("the same address registered again got id %d, which is not above the purged account's %d", got, lastID)
	}
	other, _ := e.join(e.invite(tm.owner, tm.ws, "another@example.com", RoleMember), "Another")
	if got := e.userID(other); got <= lastID {
		t.Fatalf("a new account got id %d, which the purged account %d had", got, lastID)
	}
	// Deleting the highest again, and the mark stays where it was or goes up, never down.
	var before int64
	if err := e.db.QueryRow(`SELECT high FROM user_ids`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DELETE FROM spaces WHERE owner_id = ?`, `DELETE FROM users WHERE id = ?`} {
		if _, err := e.db.Exec(q, e.userID(other)); err != nil {
			t.Fatal(err)
		}
	}
	var after int64
	if err := e.db.QueryRow(`SELECT high FROM user_ids`).Scan(&after); err != nil || after < before {
		t.Fatalf("the mark went from %d to %d, %v", before, after, err)
	}
}

// The route checks that the caller is an operator before its handler runs; the handler checks again
// in the transaction that cancels, so an operator demoted in between can't cancel.
func TestCancelingADeletionChecksTheOperatorAgainInItsTransaction(t *testing.T) {
	tm, _ := newDeletionTeam(t)
	e := tm.e
	expect(t, tm.member.do(http.MethodPost, tm.deletionPath(), map[string]string{"password": otherPassword}), http.StatusAccepted)
	adminElsewhere, _ := e.join(e.invite(tm.owner, tm.ws, "second@example.com", RoleMember), "Second")
	secondID := e.userID(adminElsewhere) // not an operator: only the setup workspace's admins are

	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	req.SetPathValue("user", strconv.FormatInt(tm.memberID, 10))
	req = req.WithContext(context.WithValue(req.Context(), userKey{}, User{ID: secondID}))
	rec := httptest.NewRecorder()
	e.svc.handleCancelDeletion(rec, req)
	expect(t, rec, http.StatusForbidden)
	if got := errCode(t, rec); got != string(apierr.CodeOperatorOnly) {
		t.Fatalf("code = %q", got)
	}
	if n := e.count(`SELECT count(*) FROM account_deletions`); n != 1 {
		t.Fatal("a caller who isn't an operator cancelled the deletion")
	}
}

// A deletion's answer that arrives after another account signed in on the same browser must not
// expire that account's cookie: the answer sets no session cookie, and the deleted account's
// sessions are already revoked.
func TestADelayedDeletionAnswerLeavesANewerSessionsCookie(t *testing.T) {
	tm, _ := newDeletionTeam(t)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse("https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	// The browser holds the member's cookie when the deletion is sent.
	jar.SetCookies(u, []*http.Cookie{tm.member.cookie})
	req := tm.member.req(http.MethodPost, tm.deletionPath(), map[string]string{"password": otherPassword})
	rec := httptest.NewRecorder()
	tm.e.h.ServeHTTP(rec, req)
	expect(t, rec, http.StatusAccepted)
	// Before the answer arrives, the owner signs in here: the jar holds the owner's cookie.
	jar.SetCookies(u, []*http.Cookie{tm.owner.cookie})
	jar.SetCookies(u, rec.Result().Cookies())
	var held string
	for _, c := range jar.Cookies(u) {
		if c.Name == tm.owner.cookie.Name {
			held = c.Value
		}
	}
	if held != tm.owner.cookie.Value {
		t.Fatalf("the owner's cookie is %q after the old answer, want it kept", held)
	}
	// And the deleted account's own cookie opens nothing.
	expect(t, tm.member.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
}
