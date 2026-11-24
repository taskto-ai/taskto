package auth

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// A reset link is not a second factor: for an account with two-factor login on, using one
// sets the password and ends every session, and then asks for a code like a login does.
func TestResetLinkAsksForACodeWhenTwoFactorIsOn(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	member, memberMe := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	two := e.turnOn(member, otherPassword)
	pending := e.client().challenge("member@example.com", otherPassword)
	link := e.reset(owner, ws, memberMe.User.ID)

	browser := e.client()
	rec := browser.do(http.MethodPost, "/api/password-reset", map[string]string{"token": link, "password": newPassword})
	expect(t, rec, http.StatusOK)
	got := decode[challengeReply](t, rec)
	if !got.Required || got.Challenge == "" || got.ExpiresAt == "" {
		t.Fatalf("reset reply = %s, want a challenge", rec.Body.String())
	}
	if browser.cookie != nil {
		t.Fatal("the reset link made a session for an account with two-factor login on")
	}
	if n := e.count(`SELECT count(*) FROM sessions WHERE user_id = ?`, memberMe.User.ID); n != 0 {
		t.Fatalf("%d sessions after the reset, want none until the code", n)
	}
	expect(t, browser.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	expect(t, member.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	// No session means no access tokens either.
	expect(t, browser.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/tokens", ws), map[string]string{"name": "cli"}), http.StatusUnauthorized)
	// The half-done login from before the reset is gone; the link is used up.
	expect(t, e.client().second(pending, e.codeNow(two.secret)), http.StatusUnauthorized)
	expect(t, e.client().do(http.MethodPost, "/api/password-reset", map[string]string{"token": link, "password": "fake-third-password"}), http.StatusNotFound)

	// The second step is the login's: a wrong code leaves it for another try, a right one signs in.
	expect(t, browser.second(got.Challenge, wrongCode), http.StatusUnauthorized)
	if browser.cookie != nil {
		t.Fatal("a wrong code made a session")
	}
	expect(t, browser.second(got.Challenge, e.codeNow(two.secret)), http.StatusOK)
	expect(t, browser.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	expect(t, browser.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/tokens", ws), map[string]string{"name": "cli"}), http.StatusCreated)
	// The new password is the one that works, and it asks for the code too.
	if _, code := e.login("member@example.com", otherPassword); code != http.StatusUnauthorized {
		t.Fatalf("the old password: %d", code)
	}
	_ = e.client().challenge("member@example.com", newPassword)
}

// A recovery code finishes a reset's second step too, once.
func TestResetLinkSecondStepTakesARecoveryCode(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	member, memberMe := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	two := e.turnOn(member, otherPassword)
	link := e.reset(owner, ws, memberMe.User.ID)
	c := e.client()
	rec := c.do(http.MethodPost, "/api/password-reset", map[string]string{"token": link, "password": newPassword})
	expect(t, rec, http.StatusOK)
	expect(t, c.second(decode[challengeReply](t, rec).Challenge, two.codes[0]), http.StatusOK)
	if n := e.count(`SELECT count(*) FROM user_recovery_codes WHERE used_at IS NOT NULL`); n != 1 {
		t.Fatalf("used recovery codes = %d", n)
	}
}

// Without two-factor login a reset link still signs in, as before.
func TestResetLinkSignsInWithoutTwoFactor(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	_, memberMe := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	c := e.client()
	rec := c.do(http.MethodPost, "/api/password-reset", map[string]string{"token": e.reset(owner, ws, memberMe.User.ID), "password": newPassword})
	expect(t, rec, http.StatusOK)
	if c.cookie == nil {
		t.Fatal("no session")
	}
	expect(t, c.do(http.MethodGet, "/api/me", nil), http.StatusOK)
}

// Enrolling while the password is being checked: the login reads the two-factor state where it
// issues what the password earns, so it can't hand out a session that never asked for the code.
func TestLoginDuringEnrolmentGetsAChallenge(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	e.svc.hook = func(point string) {
		if point != "password-verified" {
			return
		}
		e.svc.hook = nil
		e.turnOn(owner, ownerPassword)
	}
	c := e.client()
	rec := c.login("owner@example.com", ownerPassword)
	expect(t, rec, http.StatusOK)
	if got := decode[challengeReply](t, rec); !got.Required || got.Challenge == "" {
		t.Fatalf("login reply = %s, want a challenge", rec.Body.String())
	}
	if c.cookie != nil {
		t.Fatal("a password-only session was issued after two-factor login was turned on")
	}
}

// A password changed (or reset) while a login is checking the old one: the login fails, it
// makes neither a session nor a challenge the change should have ended.
func TestLoginDuringPasswordChangeFails(t *testing.T) {
	for _, withTwoFactor := range []bool{true, false} {
		t.Run(fmt.Sprintf("two-factor=%v", withTwoFactor), func(t *testing.T) {
			e := newEnv(t, Options{})
			owner, _ := e.setup()
			if withTwoFactor {
				e.turnOn(owner, ownerPassword)
			}
			e.svc.hook = func(point string) {
				if point != "password-verified" {
					return
				}
				e.svc.hook = nil
				expect(t, owner.do(http.MethodPost, "/api/me/password",
					map[string]string{"current_password": ownerPassword, "new_password": renewedPassword}), http.StatusNoContent)
			}
			c := e.client()
			rec := c.login("owner@example.com", ownerPassword)
			expect(t, rec, http.StatusUnauthorized)
			if got := errCode(t, rec); got != "wrong_credentials" {
				t.Fatalf("code = %s", got)
			}
			if c.cookie != nil {
				t.Fatal("a session for the old password")
			}
			if n := e.count(`SELECT count(*) FROM login_challenges`); n != 0 {
				t.Fatalf("%d challenges survived the password change", n)
			}
			// Only the session the change itself made is there (the sweep of expired ones aside).
			if n := e.count(`SELECT count(*) FROM sessions`); n != 1 {
				t.Fatalf("sessions = %d", n)
			}
		})
	}
}

// The second step consumes its challenge by token hash and user, never by a row id: a
// replacement row that took the id of the one that was used meanwhile is left alone.
func TestSecondStepDoesNotConsumeAReplacementRow(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	two := e.turnOn(owner, ownerPassword)
	c := e.client()
	ch := c.challenge("owner@example.com", ownerPassword)
	replacement := hashToken("fake-replacement-challenge")
	e.svc.hook = func(point string) {
		if point != "challenge-found" {
			return
		}
		e.svc.hook = nil
		// Another request used this challenge up, and a new login took its row id.
		if _, err := e.db.Exec(`DELETE FROM login_challenges`); err != nil {
			t.Error(err)
		}
		if _, err := e.db.Exec(`INSERT INTO login_challenges (token_hash, user_id, created_at, expires_at) VALUES (?, 1, ?, ?)`,
			replacement, stamp(e.now), stamp(e.now.Add(challengeTTL))); err != nil {
			t.Error(err)
		}
	}
	rec := c.second(ch, e.codeNow(two.secret))
	expect(t, rec, http.StatusUnauthorized)
	if got := errCode(t, rec); got != "two_factor_challenge_invalid" {
		t.Fatalf("code = %s", got)
	}
	if c.cookie != nil {
		t.Fatal("a session from a challenge that was already used")
	}
	if n := e.count(`SELECT count(*) FROM login_challenges WHERE token_hash = ?`, replacement); n != 1 {
		t.Fatal("the replacement challenge was consumed")
	}
}

// A challenge that has used its attempts takes no more, not even a right code, whatever else
// happened to it: the count is read in the transaction that checks the code.
func TestSecondStepChecksAttemptsWhereItChecksTheCode(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	two := e.turnOn(owner, ownerPassword)
	c := e.client()
	ch := c.challenge("owner@example.com", ownerPassword)
	// Counted by a request that hasn't dropped the challenge yet.
	if _, err := e.db.Exec(`UPDATE login_challenges SET attempts = ?`, challengeAttempts); err != nil {
		t.Fatal(err)
	}
	rec := c.second(ch, e.codeNow(two.secret))
	expect(t, rec, http.StatusUnauthorized)
	if got := errCode(t, rec); got != "two_factor_challenge_invalid" {
		t.Fatalf("code = %s", got)
	}
	if c.cookie != nil {
		t.Fatal("a session from a challenge with no attempts left")
	}
}

// Wrong codes sent at once count against one challenge's attempts, no more than
// challengeAttempts of them are tried, and the wrong ones are counted even though they fail.
func TestSecondStepConcurrentWrongCodesAreCappedAtTheAttempts(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	two := e.turnOn(owner, ownerPassword)
	ch := e.client().challenge("owner@example.com", ownerPassword)

	const requests = 12
	codes := make([]int, requests)
	reasons := make([]string, requests)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each from an address of its own, so the code limits don't decide it.
			c := e.client()
			c.ip = fmt.Sprintf("192.0.2.%d:1234", 10+i)
			rec := c.second(ch, wrongCode)
			codes[i], reasons[i] = rec.Code, errCode(t, rec)
		}()
	}
	wg.Wait()
	wrong := 0
	for i := range requests {
		if codes[i] != http.StatusUnauthorized {
			t.Fatalf("request %d: status %d", i, codes[i])
		}
		if reasons[i] == "wrong_two_factor_code" {
			wrong++
		}
	}
	if wrong != challengeAttempts {
		t.Fatalf("%d wrong codes were tried, want %d", wrong, challengeAttempts)
	}
	if n := e.count(`SELECT count(*) FROM login_challenges`); n != 0 {
		t.Fatal("the used-up challenge is still there")
	}
	c := e.client()
	expect(t, c.second(ch, e.codeNow(two.secret)), http.StatusUnauthorized)
}

// wrongCode is neither a six-digit code nor a recovery code, so it can't match whatever the
// random secret of a test makes: it is the wrong code in every test that needs one.
const wrongCode = "abcdef"

// expectSession logs in with a password alone, which must give a session and no code step.
func expectSession(t *testing.T, c *client, email, password string) {
	t.Helper()
	rec := c.login(email, password)
	expect(t, rec, http.StatusOK)
	if got := decode[map[string]any](t, rec); got["two_factor_required"] != nil || got["user"] == nil {
		t.Fatalf("login reply = %s, want a signed-in user", rec.Body.String())
	}
	if c.cookie == nil {
		t.Fatal("no session cookie")
	}
	expect(t, c.do(http.MethodGet, "/api/me", nil), http.StatusOK)
}

type enrolled struct {
	Enrolment string `json:"enrolment"`
	Secret    string `json:"secret"`
}

func (e *env) enrol(c *client, password string) (enrolled, []byte) {
	e.t.Helper()
	rec := c.do(http.MethodPost, "/api/me/two-factor/enrol", map[string]string{"password": password})
	expect(e.t, rec, http.StatusOK)
	got := decode[enrolled](e.t, rec)
	secret, err := secretEncoding.DecodeString(got.Secret)
	if err != nil {
		e.t.Fatal(err)
	}
	return got, secret
}

func (e *env) confirmStatus(c *client, enrolment, code string) (int, string) {
	e.t.Helper()
	rec := c.do(http.MethodPost, "/api/me/two-factor/confirm", map[string]string{"enrolment": enrolment, "code": code})
	if rec.Code == http.StatusOK {
		return rec.Code, ""
	}
	return rec.Code, errCode(e.t, rec)
}

// A code confirms the enrolment that showed its secret: a second enrolment (another tab's)
// makes a new id along with the new secret, and the first can no longer be confirmed.
func TestConfirmIsBoundToItsEnrolment(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	first, firstSecret := e.enrol(owner, ownerPassword)
	second, secondSecret := e.enrol(owner, ownerPassword)
	if first.Enrolment == "" || first.Enrolment == second.Enrolment {
		t.Fatalf("enrolment ids %q, %q", first.Enrolment, second.Enrolment)
	}

	// The first tab's id with its own code, with the new secret's code, or with none.
	for _, c := range []struct{ enrolment, code string }{
		{first.Enrolment, e.codeNow(firstSecret)},
		{first.Enrolment, e.codeNow(secondSecret)},
		{"", e.codeNow(secondSecret)},
		{"fake-unknown-enrolment", e.codeNow(secondSecret)},
	} {
		if status, code := e.confirmStatus(owner, c.enrolment, c.code); status != http.StatusConflict || code != "two_factor_not_started" {
			t.Fatalf("confirm(%q) = %d %s", c.enrolment, status, code)
		}
	}
	if n := e.count(`SELECT count(*) FROM user_totp WHERE enabled_at IS NOT NULL`); n != 0 {
		t.Fatal("a stale enrolment turned two-factor login on")
	}
	if status, code := e.confirmStatus(owner, second.Enrolment, e.codeNow(secondSecret)); status != http.StatusOK {
		t.Fatalf("confirm of the current enrolment = %d %s", status, code)
	}
	// Confirmed, the id is gone.
	if n := e.count(`SELECT count(*) FROM user_totp WHERE enrolment_id IS NOT NULL`); n != 0 {
		t.Fatal("the enrolment id outlived the enrolment")
	}
}

func TestEnrolmentExpires(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	got, secret := e.enrol(owner, ownerPassword)
	e.advance(enrolmentTTL + time.Second)
	if status, code := e.confirmStatus(owner, got.Enrolment, e.codeNow(secret)); status != http.StatusConflict || code != "two_factor_not_started" {
		t.Fatalf("confirm after the enrolment expired = %d %s", status, code)
	}
	// Asking again makes a new one that works.
	again, secret := e.enrol(owner, ownerPassword)
	if status, code := e.confirmStatus(owner, again.Enrolment, e.codeNow(secret)); status != http.StatusOK {
		t.Fatalf("confirm of a new enrolment = %d %s", status, code)
	}
}

// An enrolment that wasn't confirmed doesn't outlive a change of the password, or a reset.
func TestPasswordChangeAndResetDropAPendingEnrolment(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	member, memberMe := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")

	got, secret := e.enrol(owner, ownerPassword)
	expect(t, owner.do(http.MethodPost, "/api/me/password",
		map[string]string{"current_password": ownerPassword, "new_password": renewedPassword}), http.StatusNoContent)
	if status, code := e.confirmStatus(owner, got.Enrolment, e.codeNow(secret)); status != http.StatusConflict || code != "two_factor_not_started" {
		t.Fatalf("confirm after a password change = %d %s", status, code)
	}

	got, secret = e.enrol(member, otherPassword)
	c := e.client()
	expect(t, c.do(http.MethodPost, "/api/password-reset",
		map[string]string{"token": e.reset(owner, ws, memberMe.User.ID), "password": newPassword}), http.StatusOK)
	if n := e.count(`SELECT count(*) FROM user_totp WHERE user_id = ?`, memberMe.User.ID); n != 0 {
		t.Fatal("a pending enrolment survived a reset")
	}
	if status, code := e.confirmStatus(c, got.Enrolment, e.codeNow(secret)); status != http.StatusConflict || code != "two_factor_not_started" {
		t.Fatalf("confirm after a reset = %d %s", status, code)
	}
	// Two-factor login that was on stays on through a change of the password.
	e.turnOn(c, newPassword)
	expect(t, c.do(http.MethodPost, "/api/me/password",
		map[string]string{"current_password": newPassword, "new_password": renewedPassword}), http.StatusNoContent)
	if n := e.count(`SELECT count(*) FROM user_totp WHERE user_id = ? AND enabled_at IS NOT NULL`, memberMe.User.ID); n != 1 {
		t.Fatal("a password change turned two-factor login off")
	}
}

// A challenge made before the operator turned passwords off finishes no login.
func TestSecondStepRefusedWhenPasswordLoginIsOff(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	two := e.turnOn(owner, ownerPassword)
	c := e.client()
	ch := c.challenge("owner@example.com", ownerPassword)
	e.svc.opts.DisablePasswordLogin = true
	rec := c.second(ch, e.codeNow(two.secret))
	expect(t, rec, http.StatusForbidden)
	if got := errCode(t, rec); got != "password_login_disabled" {
		t.Fatalf("code = %s", got)
	}
	if c.cookie != nil {
		t.Fatal("a session from a challenge of the passwords that are off")
	}
}

// A second step the account's limit refuses tried no code: it isn't charged to the address,
// whose budget others behind it share.
func TestSecondStepAccountLimitDoesNotChargeTheAddress(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	e.turnOn(owner, ownerPassword)
	c := e.client()
	for range codeAccountAndIPAttempts {
		expect(t, c.second(c.challenge("owner@example.com", ownerPassword), wrongCode), http.StatusUnauthorized)
	}
	used := func() int {
		e.svc.codePerIP.mu.Lock()
		defer e.svc.codePerIP.mu.Unlock()
		n := 0
		for _, w := range e.svc.codePerIP.hits {
			n += w.count
		}
		return n
	}
	before := used()
	expect(t, c.second(c.challenge("owner@example.com", ownerPassword), wrongCode), http.StatusTooManyRequests)
	if got := used(); got != before {
		t.Fatalf("the address was charged %d for a refused attempt", got-before)
	}
}

// Enrolling is checked against the password that was verified: one changed meanwhile makes no
// enrolment, whose secret the old password's holder would otherwise have.
func TestEnrolDuringPasswordChangeFails(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	e.svc.hook = func(point string) {
		if point != "own-password-verified" {
			return
		}
		e.svc.hook = nil
		expect(t, owner.do(http.MethodPost, "/api/me/password",
			map[string]string{"current_password": ownerPassword, "new_password": renewedPassword}), http.StatusNoContent)
	}
	rec := owner.do(http.MethodPost, "/api/me/two-factor/enrol", map[string]string{"password": ownerPassword})
	expect(t, rec, http.StatusConflict)
	if got := errCode(t, rec); got != "password_changed" {
		t.Fatalf("code = %s", got)
	}
	if n := e.count(`SELECT count(*) FROM user_totp`); n != 0 {
		t.Fatal("an enrolment was made for a password that changed")
	}
}

// Turning it off, or making new recovery codes, with a password that was changed meanwhile: the
// request doesn't outlive the session the change ended.
func TestFactorActionDuringPasswordChangeFails(t *testing.T) {
	for _, path := range []string{"/api/me/two-factor/disable", "/api/me/two-factor/recovery-codes"} {
		t.Run(path, func(t *testing.T) {
			e := newEnv(t, Options{})
			owner, _ := e.setup()
			two := e.turnOn(owner, ownerPassword)
			e.svc.hook = func(point string) {
				if point != "own-password-verified" {
					return
				}
				e.svc.hook = nil
				if _, err := e.db.Exec(`UPDATE users SET password_hash = ?`, "$changed"); err != nil {
					t.Error(err)
				}
			}
			rec := owner.do(http.MethodPost, path, map[string]string{"password": ownerPassword, "code": e.codeNow(two.secret)})
			expect(t, rec, http.StatusConflict)
			if got := errCode(t, rec); got != "password_changed" {
				t.Fatalf("code = %s", got)
			}
			if n := e.count(`SELECT count(*) FROM user_totp WHERE enabled_at IS NOT NULL`); n != 1 {
				t.Fatal("two-factor login was turned off by a request that outlived its password")
			}
			if n := e.count(`SELECT count(*) FROM user_recovery_codes WHERE used_at IS NULL`); n != recoveryCodeCount {
				t.Fatalf("recovery codes left = %d", n)
			}
		})
	}
}

// A reset link that earns a challenge leaves the browser's own session alone until the code.
func TestResetChallengeKeepsTheBrowsersSession(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	member, memberMe := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	e.turnOn(member, otherPassword)
	link := e.reset(owner, ws, memberMe.User.ID)
	// The owner's browser redeems the member's link.
	rec := owner.do(http.MethodPost, "/api/password-reset", map[string]string{"token": link, "password": newPassword})
	expect(t, rec, http.StatusOK)
	expect(t, owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)
}

func TestTwoFactorStatusReportsCodesLeft(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	rec := owner.do(http.MethodGet, "/api/me/two-factor", nil)
	if got := decode[map[string]any](t, rec); got["enabled"] != false || got["recovery_codes_left"] != float64(0) {
		t.Fatalf("status = %v", got)
	}
	e.turnOn(owner, ownerPassword)
	rec = owner.do(http.MethodGet, "/api/me/two-factor", nil)
	if got := decode[map[string]any](t, rec); got["enabled"] != true || got["recovery_codes_left"] != float64(recoveryCodeCount) {
		t.Fatalf("status = %v", got)
	}
}
