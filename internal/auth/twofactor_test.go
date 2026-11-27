package auth

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTOTPVectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, v := range []struct {
		unix int64
		want string
	}{
		{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"},
		{2000000000, "279037"}, {20000000000, "353130"},
	} {
		if got := totpCode(secret, totpStep(v.unix)); got != v.want {
			t.Errorf("code at %d = %s, want %s", v.unix, got, v.want)
		}
	}
}

func TestMatchTOTPWindowAndReplay(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := int64(1111111109)
	step := totpStep(now)
	for delta, ok := range map[int64]bool{-2: false, -1: true, 0: true, 1: true, 2: false} {
		got, matched := matchTOTP(secret, totpCode(secret, step+delta), now, 0)
		if matched != ok || (ok && got != step+delta) {
			t.Errorf("step %+d: matched %v (step %d), want %v", delta, matched, got, ok)
		}
	}
	if _, ok := matchTOTP(secret, totpCode(secret, step), now, step); ok {
		t.Error("the step last used was accepted again")
	}
	if _, ok := matchTOTP(secret, totpCode(secret, step-1), now, step); ok {
		t.Error("a step before the last used was accepted")
	}
	if _, ok := matchTOTP(secret, "12345", now, 0); ok {
		t.Error("a short code matched")
	}
}

func TestNormalizeCode(t *testing.T) {
	for in, want := range map[string]string{
		"123 456":        "123456",
		"AB12-CD34-EF56": "ab12cd34ef56",
		"OIl0-1111":      "01101111",
	} {
		if got := normalizeCode(in); got != want {
			t.Errorf("normalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
	code := newRecoveryCode()
	if len(normalizeCode(code)) != recoveryLen || normalizeCode(code) != strings.ReplaceAll(code, "-", "") {
		t.Errorf("recovery code %q doesn't survive normalizing", code)
	}
}

func TestTOTPURI(t *testing.T) {
	u, err := url.Parse(totpURI([]byte("12345678901234567890"), "a+b@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "otpauth" || u.Host != "totp" || q.Get("secret") != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" ||
		q.Get("issuer") != "taskto" || q.Get("digits") != "6" || q.Get("period") != "30" || q.Get("algorithm") != "SHA1" {
		t.Fatalf("uri = %s", u)
	}
	if got := u.Path; got != "/taskto:a+b@example.com" {
		t.Fatalf("label = %q", got)
	}
}

func (e *env) codeNow(secret []byte) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return totpCode(secret, totpStep(e.now.Unix()))
}

func (e *env) nextStep() { e.advance(totpPeriod * time.Second) }

type tfa struct {
	secret []byte
	codes  []string
}

func (e *env) turnOn(c *client, password string) tfa {
	e.t.Helper()
	rec := c.do(http.MethodPost, "/api/me/two-factor/enrol", map[string]string{"password": password})
	expect(e.t, rec, http.StatusOK)
	got := decode[struct {
		Enrolment  string `json:"enrolment"`
		Secret     string `json:"secret"`
		OtpauthURI string `json:"otpauth_uri"`
	}](e.t, rec)
	secret, err := secretEncoding.DecodeString(got.Secret)
	if err != nil || len(secret) != totpSecretBytes || !strings.Contains(got.OtpauthURI, got.Secret) || got.Enrolment == "" {
		e.t.Fatalf("enrol reply = %+v, %v", got, err)
	}
	rec = c.do(http.MethodPost, "/api/me/two-factor/confirm", map[string]string{"enrolment": got.Enrolment, "code": e.codeNow(secret)})
	expect(e.t, rec, http.StatusOK)
	codes := decode[struct {
		Codes []string `json:"recovery_codes"`
	}](e.t, rec).Codes
	if len(codes) != recoveryCodeCount {
		e.t.Fatalf("recovery codes = %d, want %d", len(codes), recoveryCodeCount)
	}
	e.nextStep()
	return tfa{secret: secret, codes: codes}
}

func (c *client) login(email, password string) *httptest.ResponseRecorder {
	return c.do(http.MethodPost, "/api/login", map[string]string{"email": email, "password": password})
}

type challengeReply struct {
	Required  bool   `json:"two_factor_required"`
	Challenge string `json:"challenge"`
	ExpiresAt string `json:"expires_at"`
}

func (c *client) challenge(email, password string) string {
	c.e.t.Helper()
	rec := c.login(email, password)
	expect(c.e.t, rec, http.StatusOK)
	got := decode[challengeReply](c.e.t, rec)
	if !got.Required || got.Challenge == "" {
		c.e.t.Fatalf("login reply = %s, want a challenge", rec.Body.String())
	}
	return got.Challenge
}

func (c *client) second(challenge, code string) *httptest.ResponseRecorder {
	return c.do(http.MethodPost, "/api/login/two-factor", map[string]string{"challenge": challenge, "code": code})
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return decode[struct {
		Code string `json:"code"`
	}](t, rec).Code
}

func TestTwoFactorEnrolment(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()

	rec := owner.do(http.MethodGet, "/api/me/two-factor", nil)
	expect(t, rec, http.StatusOK)
	if got := decode[map[string]any](t, rec); got["enabled"] != false {
		t.Fatalf("status = %v", got)
	}

	expect(t, owner.do(http.MethodPost, "/api/me/two-factor/enrol", map[string]string{"password": "fake-wrong-password"}), http.StatusForbidden)
	expect(t, owner.do(http.MethodPost, "/api/me/two-factor/enrol", map[string]string{}), http.StatusBadRequest)
	rec = owner.do(http.MethodPost, "/api/me/two-factor/confirm", map[string]string{"code": "123456"})
	expect(t, rec, http.StatusConflict)
	if got := errCode(t, rec); got != "two_factor_not_started" {
		t.Fatalf("code = %s", got)
	}

	rec = owner.do(http.MethodPost, "/api/me/two-factor/enrol", map[string]string{"password": ownerPassword})
	expect(t, rec, http.StatusOK)
	first := decode[struct{ Secret string }](t, rec).Secret
	rec = owner.do(http.MethodPost, "/api/me/two-factor/enrol", map[string]string{"password": ownerPassword})
	expect(t, rec, http.StatusOK)
	second := decode[struct{ Enrolment, Secret string }](t, rec)
	secret, err := secretEncoding.DecodeString(second.Secret)
	if err != nil || first == secretEncoding.EncodeToString(secret) {
		t.Fatalf("the second secret is the first: %v", err)
	}

	expectSession(t, e.client(), "owner@example.com", ownerPassword)
	oldCode := totpCode([]byte(mustBase32(t, first)), totpStep(e.now.Unix()))
	for _, code := range []string{wrongCode, "12345", "", oldCode} {
		rec = owner.do(http.MethodPost, "/api/me/two-factor/confirm", map[string]string{"enrolment": second.Enrolment, "code": code})
		expect(t, rec, http.StatusForbidden)
		if got := errCode(t, rec); got != "wrong_two_factor_code" {
			t.Fatalf("code %q: %s", code, got)
		}
	}
	if n := e.count(`SELECT count(*) FROM user_totp WHERE enabled_at IS NOT NULL`); n != 0 {
		t.Fatal("a wrong code turned it on")
	}

	rec = owner.do(http.MethodPost, "/api/me/two-factor/confirm", map[string]string{"enrolment": second.Enrolment, "code": e.codeNow(secret)})
	expect(t, rec, http.StatusOK)
	codes := decode[struct {
		Codes []string `json:"recovery_codes"`
	}](t, rec).Codes
	if len(codes) != recoveryCodeCount {
		t.Fatalf("recovery codes = %d", len(codes))
	}
	rec = owner.do(http.MethodGet, "/api/me/two-factor", nil)
	if got := decode[map[string]any](t, rec); got["enabled"] != true || got["recovery_codes_left"] != float64(recoveryCodeCount) {
		t.Fatalf("status = %v", got)
	}

	rec = owner.do(http.MethodPost, "/api/me/two-factor/enrol", map[string]string{"password": ownerPassword})
	expect(t, rec, http.StatusConflict)
	if got := errCode(t, rec); got != "two_factor_already_on" {
		t.Fatalf("code = %s", got)
	}
	rec = owner.do(http.MethodPost, "/api/me/two-factor/confirm", map[string]string{"code": e.codeNow(secret)})
	expect(t, rec, http.StatusConflict)

	var sealed []byte
	if err := e.db.QueryRow(`SELECT secret FROM user_totp`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, secret) || bytes.Contains(sealed, []byte(secretEncoding.EncodeToString(secret))) {
		t.Fatal("the secret is stored in clear")
	}
	if opened, err := e.svc.key.Open(sealed, e.svc.sealAD(1)); err != nil || !bytes.Equal(opened, secret) {
		t.Fatalf("the sealed secret doesn't open to the secret: %v", err)
	}
	if _, err := e.svc.key.Open(sealed, e.svc.sealAD(2)); err == nil {
		t.Fatal("the sealed secret opens for another user")
	}
	for _, code := range codes {
		if n := e.count(`SELECT count(*) FROM user_recovery_codes WHERE code_hash LIKE '%' || ? || '%'`, strings.ReplaceAll(code, "-", "")); n != 0 {
			t.Fatal("a recovery code is stored in clear")
		}
		if n := e.count(`SELECT count(*) FROM user_recovery_codes WHERE code_hash = ?`, e.svc.recoveryHash(1, strings.ReplaceAll(code, "-", ""))); n != 1 {
			t.Fatalf("recovery code %s isn't stored by its keyed hash", code)
		}
	}
	if n := e.count(`SELECT count(*) FROM pragma_table_info('users') WHERE name = 'totp_secret'`); n != 0 {
		t.Fatal("users still has the unused totp_secret column")
	}
}

func mustBase32(t *testing.T, s string) string {
	t.Helper()
	b, err := secretEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLoginAsksForACodeOnlyWithTwoFactorOn(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	token := e.invite(owner, ws, "member@example.com", RoleMember)
	e.join(token, "Member")
	two := e.turnOn(owner, ownerPassword)

	rec := e.client().login("member@example.com", otherPassword)
	expect(t, rec, http.StatusOK)
	if got := decode[me](t, rec); got.User.Email != "member@example.com" {
		t.Fatalf("login = %+v", got)
	}

	c := e.client()
	rec = c.login("owner@example.com", ownerPassword)
	expect(t, rec, http.StatusOK)
	if len(rec.Result().Cookies()) != 0 || c.cookie != nil {
		t.Fatalf("the password alone set a cookie: %v", rec.Result().Cookies())
	}
	got := decode[challengeReply](t, rec)
	if !got.Required || got.Challenge == "" || got.ExpiresAt == "" {
		t.Fatalf("login reply = %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"user"`) {
		t.Fatalf("the half-done login reply holds the user: %s", rec.Body.String())
	}
	expect(t, e.client().login("owner@example.com", "fake-wrong-password"), http.StatusUnauthorized)

	for _, name := range []string{"__Host-taskto_session", "taskto_session"} {
		half := &client{e: e, ip: c.ip, cookie: &http.Cookie{Name: name, Value: got.Challenge}}
		expect(t, half.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
		expect(t, half.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", ws), nil), http.StatusUnauthorized)
		expect(t, half.do(http.MethodGet, "/api/me/two-factor", nil), http.StatusUnauthorized)
	}
	bearer := e.client().req(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", ws), nil)
	bearer.Header.Set("Authorization", "Bearer "+got.Challenge)
	expect(t, e.client().send(bearer), http.StatusUnauthorized)
	if n := e.count(`SELECT count(*) FROM sessions WHERE user_id = 1`); n != 1 {
		t.Fatalf("owner's sessions = %d: the password alone made one", n)
	}
	if n := e.count(`SELECT count(*) FROM login_challenges WHERE token_hash = ?`, got.Challenge); n != 0 {
		t.Fatal("the challenge is stored in clear")
	}

	rec = c.second(got.Challenge, wrongCode)
	expect(t, rec, http.StatusUnauthorized)
	if code := errCode(t, rec); code != "wrong_two_factor_code" {
		t.Fatalf("code = %s", code)
	}
	if c.cookie != nil {
		t.Fatal("a wrong code set a cookie")
	}
	rec = c.second(got.Challenge, e.codeNow(two.secret))
	expect(t, rec, http.StatusOK)
	if u := decode[me](t, rec).User; u.Email != "owner@example.com" {
		t.Fatalf("second step = %+v", u)
	}
	expect(t, c.do(http.MethodGet, "/api/me", nil), http.StatusOK)

	rec = e.client().second(got.Challenge, e.codeNow(two.secret))
	expect(t, rec, http.StatusUnauthorized)
	if code := errCode(t, rec); code != "two_factor_challenge_invalid" {
		t.Fatalf("code = %s", code)
	}
}

func TestSecondStepBrowserKeepsItsSessionUntilItPasses(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	two := e.turnOn(owner, ownerPassword)
	before := owner.cookie
	challenge := owner.challenge("owner@example.com", ownerPassword)
	expect(t, owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	expect(t, owner.second(challenge, wrongCode), http.StatusUnauthorized)
	expect(t, owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	expect(t, owner.second(challenge, e.codeNow(two.secret)), http.StatusOK)
	stale := &client{e: e, ip: owner.ip, cookie: before}
	expect(t, stale.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
}

func TestTOTPCodeWorksOnce(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	two := e.turnOn(owner, ownerPassword)

	code := e.codeNow(two.secret)
	expect(t, e.client().second(e.client().challenge("owner@example.com", ownerPassword), code), http.StatusOK)

	c := e.client()
	rec := c.second(c.challenge("owner@example.com", ownerPassword), code)
	expect(t, rec, http.StatusUnauthorized)
	if got := errCode(t, rec); got != "wrong_two_factor_code" {
		t.Fatalf("code = %s", got)
	}
	earlier := totpCode(two.secret, totpStep(e.now.Unix())-1)
	c = e.client()
	expect(t, c.second(c.challenge("owner@example.com", ownerPassword), earlier), http.StatusUnauthorized)
	later := totpCode(two.secret, totpStep(e.now.Unix())+1)
	c = e.client()
	expect(t, c.second(c.challenge("owner@example.com", ownerPassword), later), http.StatusOK)
	c = e.client()
	expect(t, c.second(c.challenge("owner@example.com", ownerPassword), e.codeNow(two.secret)), http.StatusUnauthorized)
	c = e.client()
	e.advance(2 * totpPeriod * time.Second)
	expect(t, c.second(c.challenge("owner@example.com", ownerPassword), totpCode(two.secret, totpStep(e.now.Unix())-3)), http.StatusUnauthorized)
	expect(t, c.second(c.challenge("owner@example.com", ownerPassword), e.codeNow(two.secret)), http.StatusOK)
}

func TestTOTPCodeIsSpentOnceWhenSentTwiceAtOnce(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	two := e.turnOn(owner, ownerPassword)
	c1, c2 := e.client(), e.client()
	ch1, ch2 := c1.challenge("owner@example.com", ownerPassword), c2.challenge("owner@example.com", ownerPassword)
	code := e.codeNow(two.secret)
	results := make(chan int, 2)
	for _, p := range []struct {
		c  *client
		ch string
	}{{c1, ch1}, {c2, ch2}} {
		go func() { results <- p.c.second(p.ch, code).Code }()
	}
	a, b := <-results, <-results
	if (a != http.StatusOK || b != http.StatusUnauthorized) && (a != http.StatusUnauthorized || b != http.StatusOK) {
		t.Fatalf("two requests with one code got %d and %d, want one success", a, b)
	}
}

func TestRecoveryCodesWorkOnce(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	two := e.turnOn(owner, ownerPassword)

	use := func(code string) *httptest.ResponseRecorder {
		c := e.client()
		return c.second(c.challenge("owner@example.com", ownerPassword), code)
	}
	expect(t, use(two.codes[0]), http.StatusOK)
	rec := use(two.codes[0])
	expect(t, rec, http.StatusUnauthorized)
	if got := errCode(t, rec); got != "wrong_two_factor_code" {
		t.Fatalf("code = %s", got)
	}
	expect(t, use(strings.ToUpper(strings.ReplaceAll(two.codes[0], "-", ""))), http.StatusUnauthorized)
	expect(t, use(strings.ToUpper(strings.ReplaceAll(two.codes[1], "-", " "))), http.StatusOK)
	expect(t, use(two.codes[1]), http.StatusUnauthorized)

	expect(t, use(two.codes[2]), http.StatusOK)
	expect(t, use(e.codeNow(two.secret)), http.StatusOK)

	rec = owner.do(http.MethodGet, "/api/me/two-factor", nil)
	if got := decode[map[string]any](t, rec); got["recovery_codes_left"] != float64(recoveryCodeCount-3) {
		t.Fatalf("status = %v", got)
	}
	for _, code := range []string{"0000-0000-0000", "zzzz-zzzz-zzzz", "abc", strings.Repeat("a", 65)} {
		expect(t, use(code), map[bool]int{true: http.StatusBadRequest, false: http.StatusUnauthorized}[len(code) > 64])
	}
}

func TestRecoveryCodesDontConfirmEnrolment(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	two := e.turnOn(owner, ownerPassword)
	expect(t, owner.do(http.MethodPost, "/api/me/two-factor/confirm", map[string]string{"code": two.codes[0]}), http.StatusConflict)
}

func TestSecondStepChallengeExpiresAndTakesFewAttempts(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	two := e.turnOn(owner, ownerPassword)

	c := e.client()
	ch := c.challenge("owner@example.com", ownerPassword)
	e.advance(challengeTTL)
	rec := c.second(ch, e.codeNow(two.secret))
	expect(t, rec, http.StatusUnauthorized)
	if got := errCode(t, rec); got != "two_factor_challenge_invalid" {
		t.Fatalf("expired challenge: %s", got)
	}
	_ = c.challenge("owner@example.com", ownerPassword)
	if n := e.count(`SELECT count(*) FROM login_challenges`); n != 1 {
		t.Fatalf("challenges = %d, want the new one only", n)
	}

	ch = c.challenge("owner@example.com", ownerPassword)
	for i := range challengeAttempts {
		rec := c.second(ch, wrongCode)
		expect(t, rec, http.StatusUnauthorized)
		if got := errCode(t, rec); got != "wrong_two_factor_code" {
			t.Fatalf("wrong code %d: %s", i+1, got)
		}
	}
	rec = c.second(ch, e.codeNow(two.secret))
	expect(t, rec, http.StatusUnauthorized)
	if got := errCode(t, rec); got != "two_factor_challenge_invalid" {
		t.Fatalf("used-up challenge: %s", got)
	}
	for _, ch := range []string{"", "made-up", strings.Repeat("x", 200)} {
		rec = c.second(ch, "123456")
		expect(t, rec, http.StatusUnauthorized)
		if got := errCode(t, rec); got != "two_factor_challenge_invalid" {
			t.Fatalf("challenge %q: %s", ch, got)
		}
	}
	expect(t, c.do(http.MethodPost, "/api/login/two-factor", map[string]string{"challenge": ch}), http.StatusBadRequest)
}

func TestSecondStepRateLimit(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	two := e.turnOn(owner, ownerPassword)

	c := e.client()
	ch := c.challenge("owner@example.com", ownerPassword)
	for i := range codeAccountAndIPAttempts {
		if i > 0 && i%(challengeAttempts-1) == 0 {
			ch = c.challenge("owner@example.com", ownerPassword)
		}
		expect(t, c.second(ch, wrongCode), http.StatusUnauthorized)
	}
	ch = c.challenge("owner@example.com", ownerPassword)
	rec := c.second(ch, e.codeNow(two.secret))
	expect(t, rec, http.StatusTooManyRequests)
	if ra := rec.Header().Get("Retry-After"); ra != "870" {
		t.Fatalf("Retry-After = %q, want 870", ra)
	}
	if got := errCode(t, rec); got != "too_many_two_factor_codes" {
		t.Fatalf("code = %s", got)
	}
	other := e.client()
	other.ip = "198.51.100.7:1"
	expect(t, other.second(other.challenge("owner@example.com", ownerPassword), e.codeNow(two.secret)), http.StatusOK)
	e.advance(attemptWindow)
	ch = c.challenge("owner@example.com", ownerPassword)
	e.nextStep()
	expect(t, c.second(ch, e.codeNow(two.secret)), http.StatusOK)
}

func TestSecondStepRateLimitPerAddressAndAccount(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	e.turnOn(owner, ownerPassword)

	c := e.client()
	for range codeIPAttempts {
		expect(t, c.second("made-up", "123456"), http.StatusUnauthorized)
	}
	rec := c.second("made-up", "123456")
	expect(t, rec, http.StatusTooManyRequests)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}

	e.advance(attemptWindow)
	for i := range codeAccountAttempts {
		c := e.client()
		c.ip = fmt.Sprintf("198.51.100.%d:1", i)
		expect(t, c.second(c.challenge("owner@example.com", ownerPassword), wrongCode), http.StatusUnauthorized)
	}
	c = e.client()
	c.ip = "203.0.113.9:1"
	expect(t, c.second(c.challenge("owner@example.com", ownerPassword), wrongCode), http.StatusTooManyRequests)
}

func TestTurnOffAndNewRecoveryCodesNeedPasswordAndCode(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	two := e.turnOn(owner, ownerPassword)
	disable := func(password, code string) *httptest.ResponseRecorder {
		return owner.do(http.MethodPost, "/api/me/two-factor/disable", map[string]string{"password": password, "code": code})
	}
	regen := func(password, code string) *httptest.ResponseRecorder {
		return owner.do(http.MethodPost, "/api/me/two-factor/recovery-codes", map[string]string{"password": password, "code": code})
	}

	for _, f := range []func(string, string) *httptest.ResponseRecorder{disable, regen} {
		expect(t, f("fake-wrong-password", e.codeNow(two.secret)), http.StatusForbidden)
		expect(t, f("", e.codeNow(two.secret)), http.StatusBadRequest)
		rec := f(ownerPassword, wrongCode)
		expect(t, rec, http.StatusForbidden)
		if got := errCode(t, rec); got != "wrong_two_factor_code" {
			t.Fatalf("code = %s", got)
		}
		expect(t, f(ownerPassword, ""), http.StatusForbidden)
	}
	rec := regen(ownerPassword, e.codeNow(two.secret))
	expect(t, rec, http.StatusOK)
	fresh := decode[struct {
		Codes []string `json:"recovery_codes"`
	}](t, rec).Codes
	if len(fresh) != recoveryCodeCount || fresh[0] == two.codes[0] {
		t.Fatalf("new recovery codes = %v", fresh)
	}
	expect(t, disable(ownerPassword, e.codeNow(two.secret)), http.StatusForbidden)

	c := e.client()
	expect(t, c.second(c.challenge("owner@example.com", ownerPassword), two.codes[0]), http.StatusUnauthorized)
	c = e.client()
	expect(t, c.second(c.challenge("owner@example.com", ownerPassword), fresh[0]), http.StatusOK)
	if n := e.count(`SELECT count(*) FROM user_recovery_codes`); n != recoveryCodeCount {
		t.Fatalf("recovery codes stored = %d", n)
	}

	expect(t, disable(ownerPassword, fresh[1]), http.StatusNoContent)
	for _, table := range []string{"user_totp", "user_recovery_codes", "login_challenges"} {
		if n := e.count(`SELECT count(*) FROM ` + table); n != 0 {
			t.Fatalf("%s has %d rows after turning it off", table, n)
		}
	}
	expectSession(t, e.client(), "owner@example.com", ownerPassword)
	rec = disable(ownerPassword, e.codeNow(two.secret))
	expect(t, rec, http.StatusConflict)
	if got := errCode(t, rec); got != "two_factor_not_on" {
		t.Fatalf("code = %s", got)
	}
	expect(t, regen(ownerPassword, e.codeNow(two.secret)), http.StatusConflict)

	again := e.turnOn(owner, ownerPassword)
	if bytes.Equal(again.secret, two.secret) {
		t.Fatal("the secret came back")
	}
}

func TestTurnOffWrongPasswordIsLimitedLikeLogin(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	two := e.turnOn(owner, ownerPassword)
	for range accountAndIPAttempts {
		expect(t, owner.do(http.MethodPost, "/api/me/two-factor/disable",
			map[string]string{"password": "fake-wrong-password", "code": e.codeNow(two.secret)}), http.StatusForbidden)
	}
	rec := owner.do(http.MethodPost, "/api/me/two-factor/disable", map[string]string{"password": "fake-wrong-password", "code": e.codeNow(two.secret)})
	expect(t, rec, http.StatusTooManyRequests)
	if got := errCode(t, rec); got != "too_many_passwords" {
		t.Fatalf("code = %s", got)
	}
}

func TestTwoFactorOfOneUserIsNotAnotherUsers(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	ownerTwo := e.turnOn(owner, ownerPassword)

	rec := member.do(http.MethodGet, "/api/me/two-factor", nil)
	if got := decode[map[string]any](t, rec); got["enabled"] != false {
		t.Fatalf("member's status = %v", got)
	}
	for _, path := range []string{"disable", "recovery-codes"} {
		rec = member.do(http.MethodPost, "/api/me/two-factor/"+path, map[string]string{"password": otherPassword, "code": e.codeNow(ownerTwo.secret)})
		expect(t, rec, http.StatusConflict)
		rec = member.do(http.MethodPost, "/api/me/two-factor/"+path, map[string]string{"password": ownerPassword, "code": e.codeNow(ownerTwo.secret)})
		expect(t, rec, http.StatusForbidden)
	}
	expect(t, member.do(http.MethodPost, "/api/me/two-factor/disable", map[string]any{"user_id": 1, "password": otherPassword, "code": "123456"}), http.StatusBadRequest)
	expect(t, member.do(http.MethodPost, "/api/me/two-factor/confirm", map[string]string{"code": e.codeNow(ownerTwo.secret)}), http.StatusConflict)

	memberTwo := e.turnOn(member, otherPassword)
	if bytes.Equal(memberTwo.secret, ownerTwo.secret) {
		t.Fatal("two users got one secret")
	}
	if n := e.count(`SELECT count(*) FROM user_totp WHERE enabled_at IS NOT NULL`); n != 2 {
		t.Fatalf("users with it on = %d", n)
	}
	c := e.client()
	ch := c.challenge("owner@example.com", ownerPassword)
	expect(t, c.second(ch, e.codeNow(memberTwo.secret)), http.StatusUnauthorized)
	expect(t, c.second(ch, memberTwo.codes[0]), http.StatusUnauthorized)
	expect(t, c.second(ch, e.codeNow(ownerTwo.secret)), http.StatusOK)
	expect(t, owner.do(http.MethodGet, "/api/me/two-factor", nil), http.StatusOK)

	expect(t, member.do(http.MethodPost, "/api/me/two-factor/disable", map[string]string{"password": otherPassword, "code": memberTwo.codes[0]}), http.StatusNoContent)
	if n := e.count(`SELECT count(*) FROM user_totp WHERE enabled_at IS NOT NULL`); n != 1 {
		t.Fatalf("users with it on = %d, want the owner's", n)
	}
}

func TestSignedOutAndTokensCantUseTwoFactorRoutes(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	two := e.turnOn(owner, ownerPassword)
	anon := e.client()
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/me/two-factor"},
		{http.MethodPost, "/api/me/two-factor/enrol"},
		{http.MethodPost, "/api/me/two-factor/confirm"},
		{http.MethodPost, "/api/me/two-factor/disable"},
		{http.MethodPost, "/api/me/two-factor/recovery-codes"},
	} {
		body := map[string]string{"password": ownerPassword, "code": e.codeNow(two.secret)}
		expect(t, anon.do(r.method, r.path, mapOrNil(r.method, body)), http.StatusUnauthorized)
	}

	rec := owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/tokens", ws), map[string]string{"name": "cli"})
	expect(t, rec, http.StatusCreated)
	raw := decode[struct{ Token string }](t, rec).Token
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		r := e.client().req(method, path, body)
		r.Header.Set("Authorization", "Bearer "+raw)
		return e.client().send(r)
	}
	expect(t, call(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", ws), nil), http.StatusOK)
	expect(t, call(http.MethodGet, "/api/me/two-factor", nil), http.StatusForbidden)
	expect(t, call(http.MethodPost, "/api/me/two-factor/disable", map[string]string{"password": ownerPassword, "code": e.codeNow(two.secret)}), http.StatusForbidden)
	expect(t, call(http.MethodPost, "/api/login/two-factor", map[string]string{"challenge": "x", "code": "123456"}), http.StatusForbidden)
}

func mapOrNil(method string, body map[string]string) any {
	if method == http.MethodGet {
		return nil
	}
	return body
}

func TestSSOLoginHasNoSecondStep(t *testing.T) {
	s := newSSOEnv(t)
	owner, _ := s.setup()
	s.turnOn(owner, ownerPassword)

	expect(t, s.client().login("owner@example.com", ownerPassword), http.StatusOK)
	if got := decode[challengeReply](t, s.client().login("owner@example.com", ownerPassword)); !got.Required {
		t.Fatal("the password login of a user with two-factor login on asks for no code")
	}
	b := s.browser()
	s.wantLogin(b, s.signIn(b, "/boards", verified("owner@example.com")), "/boards", "owner@example.com")
}

func TestTwoFactorNeedsAPasswordLogin(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	if _, err := e.db.Exec(`UPDATE users SET password_hash = ''`); err != nil {
		t.Fatal(err)
	}
	rec := owner.do(http.MethodPost, "/api/me/two-factor/enrol", map[string]string{"password": ownerPassword})
	expect(t, rec, http.StatusForbidden)
	if got := errCode(t, rec); got != "two_factor_no_password" {
		t.Fatalf("code = %s", got)
	}

	off := newEnv(t, Options{DisablePasswordLogin: true, SSO: []SSOProvider{{ID: "p", Name: "P", Issuer: "https://idp.example.com", ClientID: "c", ClientSecret: "s"}}, PublicURL: testPublicURL})
	c, _ := off.setup()
	rec = c.do(http.MethodPost, "/api/me/two-factor/enrol", map[string]string{"password": ownerPassword})
	expect(t, rec, http.StatusForbidden)
	if got := errCode(t, rec); got != "password_login_disabled" {
		t.Fatalf("code = %s", got)
	}
}

func TestWithoutAServerKeyTwoFactorIsRefused(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	e.svc.key = nil
	expect(t, owner.do(http.MethodPost, "/api/me/two-factor/enrol", map[string]string{"password": ownerPassword}), http.StatusInternalServerError)
	if n := e.count(`SELECT count(*) FROM user_totp`); n != 0 {
		t.Fatal("a secret was stored without a key to seal it")
	}
}

func TestAdminClearsAMembersTwoFactor(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	member, memberMe := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	memberID := memberMe.User.ID
	clear := func(c *client, workspace, user int64) *httptest.ResponseRecorder {
		return c.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d/two-factor", workspace, user), nil)
	}

	rec := clear(owner, ws, memberID)
	expect(t, rec, http.StatusConflict)
	if got := errCode(t, rec); got != "two_factor_not_on" {
		t.Fatalf("code = %s", got)
	}

	two := e.turnOn(member, otherPassword)
	type list struct {
		Members []struct {
			Email     string `json:"email"`
			TwoFactor *bool  `json:"two_factor"`
		} `json:"members"`
	}
	for _, c := range []struct {
		name  string
		by    *client
		shown bool
	}{{"admin", owner, true}, {"member", member, false}} {
		rec = c.by.do(http.MethodGet, fmt.Sprintf("/api/workspaces/%d/members", ws), nil)
		expect(t, rec, http.StatusOK)
		for _, m := range decode[list](t, rec).Members {
			if (m.TwoFactor != nil) != c.shown {
				t.Fatalf("%s's list shows two_factor = %v for %s", c.name, m.TwoFactor, m.Email)
			}
			if c.shown && m.TwoFactor != nil && *m.TwoFactor != (m.Email == "member@example.com") {
				t.Fatalf("two_factor of %s = %v", m.Email, *m.TwoFactor)
			}
		}
	}

	expect(t, clear(member, ws, memberID), http.StatusForbidden)
	stranger, _ := newEnvWithSecondWorkspaceAdmin(e, "stranger@example.com")
	expect(t, clear(stranger, ws, memberID), http.StatusNotFound)
	if n := e.count(`SELECT count(*) FROM user_totp WHERE enabled_at IS NOT NULL`); n != 1 {
		t.Fatal("a refused clear cleared it")
	}
	e.turnOn(owner, ownerPassword)
	rec = clear(owner, ws, 1)
	expect(t, rec, http.StatusBadRequest)
	if got := errCode(t, rec); got != "two_factor_clear_own" {
		t.Fatalf("code = %s", got)
	}
	expect(t, clear(owner, ws, 9999), http.StatusNotFound)
	expect(t, clear(owner, 9999, memberID), http.StatusNotFound)

	pending := e.client().challenge("member@example.com", otherPassword)
	expect(t, clear(owner, ws, memberID), http.StatusNoContent)
	expect(t, member.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	rec = e.client().second(pending, e.codeNow(two.secret))
	expect(t, rec, http.StatusUnauthorized)
	if got := errCode(t, rec); got != "two_factor_challenge_invalid" {
		t.Fatalf("code = %s", got)
	}
	rec = e.client().login("member@example.com", otherPassword)
	expect(t, rec, http.StatusOK)
	if got := decode[me](t, rec); got.User.Email != "member@example.com" {
		t.Fatalf("login after the clear = %s", rec.Body.String())
	}
	if n := e.count(`SELECT count(*) FROM user_totp WHERE enabled_at IS NOT NULL`); n != 1 {
		t.Fatalf("users with it on = %d, want the owner's", n)
	}
	for _, table := range []string{"user_totp", "user_recovery_codes"} {
		if n := e.count(`SELECT count(*) FROM `+table+` WHERE user_id = ?`, memberID); n != 0 {
			t.Fatalf("%s still has the member's rows", table)
		}
	}
}

func newEnvWithSecondWorkspaceAdmin(e *env, email string) (*client, int64) {
	e.t.Helper()
	id := e.addMember(email, e.addWorkspaceID())
	if _, err := e.db.Exec(`UPDATE memberships SET role = 'admin' WHERE user_id = ?`, id); err != nil {
		e.t.Fatal(err)
	}
	c := e.client()
	expect(e.t, c.login(email, otherPassword), http.StatusOK)
	return c, id
}

func (e *env) addWorkspaceID() int64 {
	e.t.Helper()
	res, err := e.db.Exec(`INSERT INTO workspaces (name) VALUES ('Elsewhere')`)
	if err != nil {
		e.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestAdminClearsOnlyWhereTheyAdministerEveryWorkspaceOfTheMember(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws1 := e.setup()
	member, memberMe := e.join(e.invite(owner, ws1, "member@example.com", RoleMember), "Member")
	memberID := memberMe.User.ID
	e.turnOn(member, otherPassword)

	ws2 := e.addWorkspaceID()
	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, ?, 'member')`, ws2, memberID); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/workspaces/%d/members/%d/two-factor", ws1, memberID)
	rec := owner.do(http.MethodDelete, path, nil)
	expect(t, rec, http.StatusForbidden)
	if got := errCode(t, rec); got != "two_factor_clear_elsewhere" {
		t.Fatalf("code = %s", got)
	}
	expect(t, owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/members/%d/password-reset", ws1, memberID), nil), http.StatusForbidden)
	if n := e.count(`SELECT count(*) FROM user_totp WHERE user_id = ?`, memberID); n != 1 {
		t.Fatal("a refused clear removed the secret")
	}
	expect(t, member.do(http.MethodGet, "/api/me", nil), http.StatusOK)

	other := e.addMember("admin2@example.com", ws2)
	if _, err := e.db.Exec(`UPDATE memberships SET role = 'admin' WHERE user_id = ? AND workspace_id = ?`, other, ws2); err != nil {
		t.Fatal(err)
	}
	admin2 := e.client()
	expect(t, admin2.login("admin2@example.com", otherPassword), http.StatusOK)
	expect(t, admin2.do(http.MethodDelete, fmt.Sprintf("/api/workspaces/%d/members/%d/two-factor", ws2, memberID), nil), http.StatusForbidden)

	if _, err := e.db.Exec(`INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, 1, 'admin')`, ws2); err != nil {
		t.Fatal(err)
	}
	expect(t, owner.do(http.MethodDelete, path, nil), http.StatusNoContent)
	if n := e.count(`SELECT count(*) FROM user_totp WHERE user_id = ?`, memberID); n != 0 {
		t.Fatal("the clear left the secret")
	}
}

func TestClearTwoFactorRefusesTokensAndSignedOut(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	member, memberMe := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Member")
	e.turnOn(member, otherPassword)
	path := fmt.Sprintf("/api/workspaces/%d/members/%d/two-factor", ws, memberMe.User.ID)

	rec := owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/tokens", ws), map[string]string{"name": "cli"})
	raw := decode[struct{ Token string }](t, rec).Token
	r := e.client().req(http.MethodDelete, path, nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	expect(t, e.client().send(r), http.StatusForbidden)
	expect(t, e.client().do(http.MethodDelete, path, nil), http.StatusUnauthorized)
	if n := e.count(`SELECT count(*) FROM user_totp WHERE user_id = ?`, memberMe.User.ID); n != 1 {
		t.Fatal("it was cleared by a request that shouldn't have worked")
	}
}

const renewedPassword = "fake-renewed-owner-password"

func TestChangingThePasswordEndsHalfDoneLogins(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	two := e.turnOn(owner, ownerPassword)
	pending := e.client().challenge("owner@example.com", ownerPassword)
	expect(t, owner.do(http.MethodPost, "/api/me/password",
		map[string]string{"current_password": ownerPassword, "new_password": renewedPassword}), http.StatusNoContent)
	rec := e.client().second(pending, e.codeNow(two.secret))
	expect(t, rec, http.StatusUnauthorized)
	if got := errCode(t, rec); got != "two_factor_challenge_invalid" {
		t.Fatalf("code = %s", got)
	}
	c := e.client()
	expect(t, c.second(c.challenge("owner@example.com", renewedPassword), e.codeNow(two.secret)), http.StatusOK)
}
