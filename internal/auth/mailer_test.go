package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/audit/audittest"
	"taskto.ai/taskto/internal/mail"
)

type recorder struct {
	mu   sync.Mutex
	sent []mail.Message
	err  error
	hold chan struct{}
}

func (r *recorder) Send(ctx context.Context, m mail.Message) error {
	if r.hold != nil {
		select {
		case <-r.hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, m)
	return nil
}

func (r *recorder) messages() []mail.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]mail.Message(nil), r.sent...)
}

const publicURL = "https://taskto.example.com"

func mailEnv(t *testing.T, mod func(*Options)) (*env, *recorder) {
	t.Helper()
	rec := &recorder{}
	opts := Options{
		PublicURL: publicURL,
		Mail:      mail.NewService(rec, mail.Info{Host: "smtp.example.com", Port: 587, Security: "starttls", From: "taskto@example.com"}),
	}
	if mod != nil {
		mod(&opts)
	}
	e := newEnv(t, opts)
	t.Cleanup(e.svc.WaitIdle)
	return e, rec
}

func (e *env) requestReset(c *client, email string) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := c.do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": email})
	e.svc.WaitIdle()
	return rec
}

var linkRe = regexp.MustCompile(regexp.QuoteMeta(publicURL) + `/(?:reset-password|invite)#([A-Za-z0-9_-]+)`)

func tokenIn(t *testing.T, m mail.Message) string {
	t.Helper()
	found := linkRe.FindStringSubmatch(m.Text)
	if found == nil {
		t.Fatalf("no link in the message:\n%s", m.Text)
	}
	if !strings.Contains(m.HTML, found[0]) {
		t.Fatalf("the HTML alternative doesn't carry the link %q", found[0])
	}
	return found[1]
}

func TestForgottenPasswordIsResetByTheEmailedLink(t *testing.T) {
	e, sent := mailEnv(t, nil)
	logs := captureLogs(t)
	e.setup()

	browser := e.client()
	rec := browser.do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": " Owner@Example.com "})
	expect(t, rec, http.StatusAccepted)
	e.svc.WaitIdle()

	msgs := sent.messages()
	if len(msgs) != 1 || len(msgs[0].To) != 1 || msgs[0].To[0] != "owner@example.com" {
		t.Fatalf("messages = %+v", msgs)
	}
	if !strings.Contains(msgs[0].Text, "Hello Owner") || !strings.Contains(msgs[0].Text, "24 hours") {
		t.Fatalf("text = %q", msgs[0].Text)
	}
	token := tokenIn(t, msgs[0])
	if strings.Contains(logs.String(), token) {
		t.Fatalf("the log has the reset token:\n%s", logs.String())
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Fatalf("the answer has the reset token: %s", rec.Body.String())
	}
	if n := e.count(`SELECT COUNT(*) FROM password_resets WHERE token_hash = ?`, token); n != 0 {
		t.Fatal("the token is stored in the clear")
	}

	rec = browser.do(http.MethodPost, "/api/password-reset", map[string]string{"token": token, "password": newPassword})
	expect(t, rec, http.StatusOK)
	if _, code := e.login("owner@example.com", newPassword); code != http.StatusOK {
		t.Fatalf("login with the new password = %d", code)
	}
	if _, code := e.login("owner@example.com", ownerPassword); code != http.StatusUnauthorized {
		t.Fatalf("login with the old password = %d", code)
	}
	rec = e.client().do(http.MethodPost, "/api/password-reset", map[string]string{"token": token, "password": "fake-third-password"})
	expect(t, rec, http.StatusNotFound)
}

func TestResetRequestAnswersTheSameForUnknownAddresses(t *testing.T) {
	e, sent := mailEnv(t, nil)
	e.setup()
	known := e.requestReset(e.client(), "owner@example.com")
	unknown := e.requestReset(e.client(), "nobody@example.com")
	if known.Code != http.StatusAccepted || unknown.Code != http.StatusAccepted {
		t.Fatalf("statuses %d, %d", known.Code, unknown.Code)
	}
	if known.Body.String() != unknown.Body.String() {
		t.Fatalf("bodies differ: %q vs %q", known.Body.String(), unknown.Body.String())
	}
	if known.Header().Get("Retry-After") != unknown.Header().Get("Retry-After") {
		t.Fatal("headers differ")
	}
	if n := len(sent.messages()); n != 1 {
		t.Fatalf("%d messages: only the account that exists gets one", n)
	}
	rec := e.client().do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": "not an address"})
	expect(t, rec, http.StatusBadRequest)
}

func TestResetRequestDoesNotWaitForTheMailServer(t *testing.T) {
	e, sent := mailEnv(t, nil)
	e.setup()
	sent.hold = make(chan struct{})
	var release sync.Once
	open := func() { release.Do(func() { close(sent.hold) }) }
	t.Cleanup(open)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := e.client().do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": "owner@example.com"})
		expect(t, rec, http.StatusAccepted)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the answer waits for the mail server")
	}
	open()
	e.svc.WaitIdle()
	if len(sent.messages()) != 1 {
		t.Fatalf("messages = %+v", sent.messages())
	}
}

func TestResetRequestIsOffWithoutMailOrAPublicAddress(t *testing.T) {
	for name, opts := range map[string]Options{
		"no mail":          {PublicURL: publicURL},
		"no public url":    {Mail: mail.NewService(&recorder{}, mail.Info{Host: "smtp.example.com"})},
		"an empty service": {PublicURL: publicURL, Mail: mail.NewService(nil, mail.Info{})},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, opts)
			e.setup()
			for _, email := range []string{"owner@example.com", "nobody@example.com"} {
				rec := e.client().do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": email})
				expect(t, rec, http.StatusConflict)
				if !strings.Contains(rec.Body.String(), `"mail_not_configured"`) {
					t.Fatalf("body = %s", rec.Body.String())
				}
			}
			rec := e.client().do(http.MethodGet, "/api/login/methods", nil)
			expect(t, rec, http.StatusOK)
			if got := decode[map[string]any](t, rec)["reset_by_email"]; got != false {
				t.Fatalf("reset_by_email = %v", got)
			}
		})
	}
}

func TestLoginMethodsSaysWhenAResetCanBeEmailed(t *testing.T) {
	e, _ := mailEnv(t, nil)
	rec := e.client().do(http.MethodGet, "/api/login/methods", nil)
	expect(t, rec, http.StatusOK)
	if got := decode[map[string]any](t, rec)["reset_by_email"]; got != true {
		t.Fatalf("reset_by_email = %v", got)
	}
	off, sent := mailEnv(t, func(o *Options) { o.DisablePasswordLogin = true })
	rec = off.client().do(http.MethodGet, "/api/login/methods", nil)
	expect(t, rec, http.StatusOK)
	if got := decode[map[string]any](t, rec)["reset_by_email"]; got != false {
		t.Fatalf("reset_by_email with passwords off = %v", got)
	}
	rec = off.client().do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": "owner@example.com"})
	expect(t, rec, http.StatusForbidden)
	if len(sent.messages()) != 0 {
		t.Fatal("mail was sent with passwords off")
	}
}

func TestResetRequestLimitsPerEmailTheSameForEveryAddress(t *testing.T) {
	e, sent := mailEnv(t, nil)
	e.setup()
	for _, email := range []string{"owner@example.com", "nobody@example.com"} {
		for i := range resetEmailAttempts {
			c := e.client()
			c.ip = fmt.Sprintf("192.0.2.%d:1", 10+i)
			if rec := e.requestReset(c, email); rec.Code != http.StatusAccepted {
				t.Fatalf("%s request %d = %d", email, i, rec.Code)
			}
		}
		c := e.client()
		c.ip = "192.0.2.99:1"
		rec := c.do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": email})
		expect(t, rec, http.StatusTooManyRequests)
		if rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), `"too_many_resets"`) {
			t.Fatalf("%s: headers %v body %s", email, rec.Header(), rec.Body.String())
		}
	}
	e.svc.WaitIdle()
	if n := len(sent.messages()); n != resetEmailAttempts {
		t.Fatalf("%d messages, want %d: a limited request sends nothing", n, resetEmailAttempts)
	}
	if rec := e.client().do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": "other@example.com"}); rec.Code != http.StatusAccepted {
		t.Fatalf("another email = %d", rec.Code)
	}
	e.advance(resetWindow + time.Second)
	if rec := e.client().do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": "owner@example.com"}); rec.Code != http.StatusAccepted {
		t.Fatalf("after the window = %d", rec.Code)
	}
}

func TestResetRequestLimitsPerClientAddress(t *testing.T) {
	e, _ := mailEnv(t, nil)
	e.setup()
	c := e.client()
	for i := range resetIPAttempts {
		rec := e.requestReset(c, fmt.Sprintf("p%d@example.com", i))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("request %d = %d", i, rec.Code)
		}
	}
	rec := c.do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": "one-more@example.com"})
	expect(t, rec, http.StatusTooManyRequests)
	other := e.client()
	other.ip = "198.51.100.7:1"
	if rec := other.do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": "one-more@example.com"}); rec.Code != http.StatusAccepted {
		t.Fatalf("another client = %d", rec.Code)
	}
}

func TestResetRequestIsNotRefusedForSendsStillRunning(t *testing.T) {
	e, sent := mailEnv(t, nil)
	e.setup()
	for range mailConcurrency {
		e.svc.mailSlots <- struct{}{}
	}
	var drained sync.Once
	drain := func() {
		drained.Do(func() {
			for range mailConcurrency {
				<-e.svc.mailSlots
			}
		})
	}
	t.Cleanup(drain)
	known := e.client().do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": "owner@example.com"})
	unknown := e.client().do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": "nobody@example.com"})
	expect(t, known, http.StatusAccepted)
	expect(t, unknown, http.StatusAccepted)
	if known.Body.String() != unknown.Body.String() {
		t.Fatalf("bodies differ: %q vs %q", known.Body.String(), unknown.Body.String())
	}
	if n := len(sent.messages()); n != 0 {
		t.Fatalf("%d messages before a turn was free", n)
	}
	drain()
	e.svc.WaitIdle()
	if n := len(sent.messages()); n != 1 {
		t.Fatalf("%d messages after the turns were free, want 1", n)
	}
}

func TestResetRequestIsLimitedByHowManyTheServerStarts(t *testing.T) {
	e, _ := mailEnv(t, nil)
	e.setup()
	ask := func(i int, email string) *httptest.ResponseRecorder {
		c := e.client()
		c.ip = fmt.Sprintf("192.0.2.%d:1", i)
		return c.do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": email})
	}
	for i := range resetStartBurst {
		if rec := ask(i+1, fmt.Sprintf("p%d@example.com", i)); rec.Code != http.StatusAccepted {
			t.Fatalf("request %d = %d", i, rec.Code)
		}
	}
	for i, email := range []string{"owner@example.com", "nobody@example.com"} {
		rec := ask(200+i, email)
		expect(t, rec, http.StatusTooManyRequests)
		if rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), `"too_many_resets"`) {
			t.Fatalf("%s: headers %v body %s", email, rec.Header(), rec.Body.String())
		}
	}
	e.advance(resetStartEvery)
	if rec := ask(210, "owner@example.com"); rec.Code != http.StatusAccepted {
		t.Fatalf("after a token came back = %d", rec.Code)
	}
	e.svc.WaitIdle()
}

func TestAShutdownEndsTheSendsAndWaitsForThem(t *testing.T) {
	e, sent := mailEnv(t, nil)
	e.setup()
	sent.hold = make(chan struct{})
	t.Cleanup(func() {
		e.svc.stopWork()
		e.svc.WaitIdle()
	})
	expect(t, e.client().do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": "owner@example.com"}), http.StatusAccepted)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.svc.Shutdown(ctx); err != nil {
		t.Fatalf("the send wasn't ended: %v", err)
	}
	if n := len(sent.messages()); n != 0 {
		t.Fatalf("%d messages", n)
	}
	e.svc.WaitIdle()
}

func TestAShutdownWaitsOnlyUntilItsDeadline(t *testing.T) {
	e, _ := mailEnv(t, nil)
	e.setup()
	release := make(chan struct{})
	e.svc.mailWork.Add(1)
	go func() { defer e.svc.mailWork.Done(); <-release }()
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := e.svc.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want the deadline's error", err)
	}
}

func TestAnEmailKeepsTheRequestsValuesButNotItsEnd(t *testing.T) {
	e, sent := mailEnv(t, nil)
	e.setup()
	sent.hold = make(chan struct{})
	var release sync.Once
	open := func() { release.Do(func() { close(sent.hold) }) }
	t.Cleanup(open)
	type key struct{}
	reqCtx, endRequest := context.WithCancel(context.WithValue(context.Background(), key{}, "from the request"))
	var got atomic.Value
	var gotErr atomic.Value
	e.svc.background(reqCtx, func(ctx context.Context) {
		got.Store(ctx.Value(key{}))
		<-sent.hold
		gotErr.Store(fmt.Sprint(ctx.Err()))
	})
	endRequest()
	open()
	e.svc.WaitIdle()
	if got.Load() != "from the request" {
		t.Fatalf("value = %v", got.Load())
	}
	if gotErr.Load() != "<nil>" {
		t.Fatalf("the work was ended with the request: %v", gotErr.Load())
	}
}

func TestAnAccountWithoutAPasswordGetsNoResetEmail(t *testing.T) {
	e, sent := mailEnv(t, nil)
	e.setup()
	if _, err := e.db.Exec(`UPDATE users SET password_hash = '' WHERE email = 'owner@example.com'`); err != nil {
		t.Fatal(err)
	}
	rec := e.requestReset(e.client(), "owner@example.com")
	expect(t, rec, http.StatusAccepted)
	if len(sent.messages()) != 0 || e.count(`SELECT COUNT(*) FROM password_resets`) != 0 {
		t.Fatal("an account made by signing in with a provider was offered a password")
	}
}

func TestResetEmailIsInTheRecipientsLanguage(t *testing.T) {
	e, sent := mailEnv(t, nil)
	owner, ws := e.setup()
	e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Maria")

	e.requestReset(e.client(), "member@example.com")
	if m := sent.messages(); len(m) != 1 || m[0].Subject != "Reset your taskto.ai password" {
		t.Fatalf("english: %+v", m)
	}
	if _, err := e.db.Exec(`UPDATE workspaces SET locale = 'pt-BR' WHERE id = ?`, ws); err != nil {
		t.Fatal(err)
	}
	e.requestReset(e.client(), "member@example.com")
	if m := sent.messages(); len(m) != 2 || !strings.Contains(m[1].Subject, "Redefina sua senha") || !strings.Contains(m[1].Text, "Olá, Maria") {
		t.Fatalf("workspace default: %+v", m)
	}
	if _, err := e.db.Exec(`UPDATE users SET locale = 'en' WHERE email = 'member@example.com'`); err != nil {
		t.Fatal(err)
	}
	e.requestReset(e.client(), "member@example.com")
	if m := sent.messages(); len(m) != 3 || m[2].Subject != "Reset your taskto.ai password" {
		t.Fatalf("picked: %+v", m)
	}
}

func TestANewResetEmailReplacesTheEarlierLinkOnceSent(t *testing.T) {
	e, sent := mailEnv(t, nil)
	e.setup()
	e.requestReset(e.client(), "owner@example.com")
	e.requestReset(e.client(), "owner@example.com")
	msgs := sent.messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d", len(msgs))
	}
	first, second := tokenIn(t, msgs[0]), tokenIn(t, msgs[1])
	if first == second {
		t.Fatal("two requests, one link")
	}
	if n := e.count(`SELECT COUNT(*) FROM password_resets`); n != 1 {
		t.Fatalf("%d reset links stored", n)
	}
	rec := e.client().do(http.MethodPost, "/api/password-reset", map[string]string{"token": first, "password": newPassword})
	expect(t, rec, http.StatusNotFound)
	rec = e.client().do(http.MethodPost, "/api/password-reset", map[string]string{"token": second, "password": newPassword})
	expect(t, rec, http.StatusOK)
}

func TestAFailedResetEmailKeepsTheEarlierLink(t *testing.T) {
	e, sent := mailEnv(t, nil)
	owner, ws := e.setup()
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Maria")
	memberID := e.userID(member)
	adminLink := e.reset(owner, ws, memberID)

	sent.err = errors.New("the relay is down")
	rec := e.requestReset(e.client(), "member@example.com")
	expect(t, rec, http.StatusAccepted)
	if n := e.count(`SELECT COUNT(*) FROM password_resets WHERE user_id = ?`, memberID); n != 1 {
		t.Fatalf("%d reset links stored, want only the admin's", n)
	}
	sent.err = nil
	sent.hold = make(chan struct{})
	var release sync.Once
	open := func() { release.Do(func() { close(sent.hold) }) }
	t.Cleanup(open)
	expect(t, e.client().do(http.MethodPost, "/api/password-reset/request", map[string]string{"email": "member@example.com"}), http.StatusAccepted)
	deadline := time.Now().Add(10 * time.Second)
	for e.count(`SELECT COUNT(*) FROM password_resets WHERE user_id = ?`, memberID) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("the new link wasn't stored while its email was being sent")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := e.count(`SELECT COUNT(*) FROM password_resets WHERE user_id = ? AND created_by IS NOT NULL`, memberID); n != 1 {
		t.Fatal("the admin's link was removed before the relay took the new one")
	}
	open()
	e.svc.WaitIdle()
	if n := e.count(`SELECT COUNT(*) FROM password_resets WHERE user_id = ?`, memberID); n != 1 {
		t.Fatalf("%d links after the send", n)
	}
	rec = e.client().do(http.MethodPost, "/api/password-reset", map[string]string{"token": adminLink, "password": newPassword})
	expect(t, rec, http.StatusNotFound)
}

func TestReplacedLinksAreRemovedEvenWhenTheSendUsedUpItsTime(t *testing.T) {
	e, _ := mailEnv(t, nil)
	owner, ws := e.setup()
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Maria")
	memberID := e.userID(member)
	e.reset(owner, ws, memberID)
	_, _, id, err := e.svc.createSelfReset(context.Background(), memberID)
	if err != nil {
		t.Fatal(err)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.svc.dropOlderResets(ended, memberID, id); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT COUNT(*) FROM password_resets WHERE user_id = ?`, memberID); n != 1 {
		t.Fatalf("%d links left, want only the new one", n)
	}
}

func TestAFailedResetEmailLeavesTheAdminsLinkWorking(t *testing.T) {
	e, sent := mailEnv(t, nil)
	owner, ws := e.setup()
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Maria")
	adminLink := e.reset(owner, ws, e.userID(member))
	sent.err = errors.New("the relay is down")
	e.requestReset(e.client(), "member@example.com")
	rec := e.client().do(http.MethodPost, "/api/password-reset", map[string]string{"token": adminLink, "password": newPassword})
	expect(t, rec, http.StatusOK)
}

func TestARequestCannotReachAnotherPersonsLink(t *testing.T) {
	e, sent := mailEnv(t, nil)
	owner, ws := e.setup()
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Maria")
	rec := e.requestReset(member, "owner@example.com")
	expect(t, rec, http.StatusAccepted)
	msgs := sent.messages()
	if len(msgs) != 1 || msgs[0].To[0] != "owner@example.com" {
		t.Fatalf("messages = %+v", msgs)
	}
	if rec.Body.String() != "{}\n" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if strings.Contains(fmt.Sprint(msgs[0]), "member@example.com") {
		t.Fatal("the asker's address is in the message")
	}
}

func TestAFailedResetEmailIsLoggedWithoutTheLinkOrTheReply(t *testing.T) {
	e, sent := mailEnv(t, nil)
	logs := captureLogs(t)
	e.setup()
	sent.err = &mail.SendError{Stage: mail.StageRejected, Code: 550, Err: errors.New("550 relay.internal.example refused owner@example.com")}
	rec := e.requestReset(e.client(), "owner@example.com")
	expect(t, rec, http.StatusAccepted)
	out := logs.String()
	if !strings.Contains(out, "the reset email wasn't sent") || !strings.Contains(out, "smtp_code=550") || !strings.Contains(out, "stage=rejected") {
		t.Fatalf("log:\n%s", out)
	}
	if strings.Contains(out, "relay.internal.example") || strings.Contains(out, "owner@example.com") {
		t.Fatalf("the log has the relay's words or the address:\n%s", out)
	}
}

func TestResetRequestNeedsNoLoginAndRefusesTokens(t *testing.T) {
	e, _ := mailEnv(t, nil)
	owner, ws := e.setup()
	token := e.accessToken(owner, ws)
	if code := e.withToken(token, http.MethodPost, "/api/password-reset/request", map[string]string{"email": "owner@example.com"}); code != http.StatusForbidden && code != http.StatusUnauthorized {
		t.Fatalf("with an access token = %d", code)
	}
	r := e.client().req(http.MethodPost, "/api/password-reset/request", map[string]string{"email": "owner@example.com"})
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := e.client().send(r)
	expect(t, rec, http.StatusForbidden)
}

func TestAResetRequestIsAuditedOnlyForAnAccountThatHasAPassword(t *testing.T) {
	e, sent := mailEnv(t, nil)
	owner, _ := e.setup()
	ownerID := e.userID(owner)

	e.requestReset(e.client(), "nobody@example.com")
	if rows := e.events(audit.PasswordResetRequested); len(rows) != 0 {
		t.Fatalf("an unknown address was recorded: %+v", rows)
	}
	e.requestReset(e.client(), "owner@example.com")
	row := e.one(audit.PasswordResetRequested)
	wantEvent(t, row, 0, 0, audit.TargetUser, ownerID)
	if row.Details["emailed"] != true {
		t.Fatalf("details = %v", row.Details)
	}
	audittest.NoSecrets(t, e.db, map[string]string{"the reset token": tokenIn(t, sent.messages()[0]), "the address": "owner@example.com"})

	sent.err = errors.New("the relay is down")
	e.requestReset(e.client(), "owner@example.com")
	if rows := e.events(audit.PasswordResetRequested); len(rows) != 2 || rows[1].Details["emailed"] != false {
		t.Fatalf("events = %+v", rows)
	}
}
