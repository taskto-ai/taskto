package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/audit"
)

const (
	firefoxMac = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:130.0) Gecko/20100101 Firefox/130.0"
	safariIOS  = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"
)

type sessionList struct {
	Sessions []Session `json:"sessions"`
}

func (e *env) loginAs(email, password, userAgent string) *client {
	e.t.Helper()
	c := e.client()
	r := c.req(http.MethodPost, "/api/login", map[string]string{"email": email, "password": password})
	r.Header.Set("User-Agent", userAgent)
	expect(e.t, c.send(r), http.StatusOK)
	return c
}

func (e *env) sessionsOf(c *client) []Session {
	e.t.Helper()
	rec := c.do(http.MethodGet, "/api/me/sessions", nil)
	expect(e.t, rec, http.StatusOK)
	return decode[sessionList](e.t, rec).Sessions
}

func TestParseUserAgent(t *testing.T) {
	for _, tc := range []struct{ ua, browser, os string }{
		{firefoxMac, "Firefox", "macOS"},
		{safariIOS, "Safari", "iOS"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0", "Edge", "Windows"},
		{"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36", "Chrome", "Linux"},
		{"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36", "Chrome", "Android"},
		{"curl/8.5.0", "", ""},
		{"", "", ""},
	} {
		if b, o := parseUserAgent(tc.ua); b != tc.browser || o != tc.os {
			t.Errorf("parseUserAgent(%q) = %q, %q; want %q, %q", tc.ua, b, o, tc.browser, tc.os)
		}
	}
	if b, _ := parseUserAgent(strings.Repeat("x", 5000) + "Firefox/1"); b != "" {
		t.Errorf("read a User-Agent past its limit: %q", b)
	}
}

func TestSessionsListShowsDeviceAndMarksTheCurrentOne(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	phone := e.loginAs("owner@example.com", ownerPassword, safariIOS)
	e.advance(time.Hour)
	laptop := e.loginAs("owner@example.com", ownerPassword, firefoxMac)

	list := e.sessionsOf(laptop)
	if len(list) != 3 {
		t.Fatalf("got %d sessions, want 3: %+v", len(list), list)
	}
	if !list[0].Current || list[0].Browser != "Firefox" || list[0].OS != "macOS" {
		t.Fatalf("the first session should be the current one, from Firefox on macOS: %+v", list[0])
	}
	current := 0
	var sawPhone bool
	for _, x := range list {
		if x.Current {
			current++
		}
		if x.Browser == "Safari" && x.OS == "iOS" {
			sawPhone = true
		}
		if x.CreatedAt == "" || x.LastSeenAt == "" || x.ExpiresAt == "" {
			t.Errorf("a session lacks its times: %+v", x)
		}
	}
	if current != 1 || !sawPhone {
		t.Fatalf("current = %d, phone listed = %v: %+v", current, sawPhone, list)
	}
	for _, x := range e.sessionsOf(phone) {
		if x.Current != (x.Browser == "Safari") {
			t.Fatalf("the phone's own list marks the wrong session: %+v", x)
		}
	}
}

func TestSessionsNeverReturnTokensOrAddresses(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	body := owner.do(http.MethodGet, "/api/me/sessions", nil).Body.String()
	if strings.Contains(body, owner.cookie.Value) || strings.Contains(body, hashToken(owner.cookie.Value)) {
		t.Fatalf("the list holds a session token or its hash: %s", body)
	}
	for _, field := range []string{"token", "ip", "user_agent", "address"} {
		if strings.Contains(body, `"`+field) {
			t.Fatalf("the list has a %q field: %s", field, body)
		}
	}
	r := owner.req(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": ownerPassword})
	r.Header.Set("User-Agent", firefoxMac)
	expect(t, owner.send(r), http.StatusOK)
	var cols int
	if err := e.db.QueryRow(`SELECT count(*) FROM pragma_table_info('sessions')
		WHERE name LIKE '%agent%' OR name LIKE '%ip%' OR name LIKE '%addr%'`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 0 {
		t.Fatal("the sessions table keeps a user agent or an address")
	}
}

func TestSessionLastSeenIsRecordedAtMostOnceAMinute(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	created := e.sessionsOf(owner)[0].LastSeenAt

	e.advance(30 * time.Second)
	expect(t, owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	if got := e.sessionsOf(owner)[0].LastSeenAt; got != created {
		t.Fatalf("last seen moved within a minute: %s -> %s", created, got)
	}
	e.advance(31 * time.Second)
	expect(t, owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	if got := e.sessionsOf(owner)[0].LastSeenAt; got <= created {
		t.Fatalf("last seen didn't move after a minute: %s -> %s", created, got)
	}
}

func TestExpiredSessionsAreNotListed(t *testing.T) {
	e := newEnv(t, Options{SessionTTL: time.Hour})
	e.setup()
	e.advance(30 * time.Minute)
	laptop := e.loginAs("owner@example.com", ownerPassword, firefoxMac)
	e.advance(31 * time.Minute)
	list := e.sessionsOf(laptop)
	if len(list) != 1 || !list[0].Current {
		t.Fatalf("want only the live session: %+v", list)
	}
}

func TestRevokeOneSession(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	phone := e.loginAs("owner@example.com", ownerPassword, safariIOS)
	laptop := e.loginAs("owner@example.com", ownerPassword, firefoxMac)

	var phoneID string
	for _, x := range e.sessionsOf(laptop) {
		if x.Browser == "Safari" {
			phoneID = x.ID
		}
	}
	expect(t, laptop.do(http.MethodDelete, fmt.Sprintf("/api/me/sessions/%s", phoneID), nil), http.StatusNoContent)
	expect(t, phone.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	expect(t, laptop.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	rec := laptop.do(http.MethodDelete, fmt.Sprintf("/api/me/sessions/%s", phoneID), nil)
	expect(t, rec, http.StatusNotFound)
	if !strings.Contains(rec.Body.String(), `"session_not_found"`) {
		t.Fatalf("no code: %s", rec.Body.String())
	}
	expect(t, laptop.do(http.MethodDelete, "/api/me/sessions/x", nil), http.StatusNotFound)
}

func TestTheCurrentSessionIsSignedOutWithLogout(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	var id string
	for _, x := range e.sessionsOf(owner) {
		if x.Current {
			id = x.ID
		}
	}
	rec := owner.do(http.MethodDelete, fmt.Sprintf("/api/me/sessions/%s", id), nil)
	expect(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), `"session_current"`) {
		t.Fatalf("no code: %s", rec.Body.String())
	}
	expect(t, owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)
}

func TestRevokeOtherSessionsKeepsTheCurrentOne(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	a := e.loginAs("owner@example.com", ownerPassword, safariIOS)
	b := e.loginAs("owner@example.com", ownerPassword, firefoxMac)

	rec := b.do(http.MethodDelete, "/api/me/sessions", nil)
	expect(t, rec, http.StatusOK)
	if got := decode[struct{ Revoked int64 }](t, rec).Revoked; got != 2 {
		t.Fatalf("revoked = %d, want the 2 others (the setup browser and the phone)", got)
	}
	expect(t, a.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	expect(t, b.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	if list := e.sessionsOf(b); len(list) != 1 || !list[0].Current {
		t.Fatalf("want only the current session: %+v", list)
	}
}

func TestSessionsAreTheCallersOwnOnly(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	alice, _ := e.join(e.invite(owner, ws, "alice@example.com", RoleMember), "Alice")

	ownerList := e.sessionsOf(owner)
	aliceList := e.sessionsOf(alice)
	if len(ownerList) != 1 || len(aliceList) != 1 || ownerList[0].ID == aliceList[0].ID {
		t.Fatalf("owner %+v, alice %+v", ownerList, aliceList)
	}

	rec := alice.do(http.MethodDelete, fmt.Sprintf("/api/me/sessions/%s", ownerList[0].ID), nil)
	expect(t, rec, http.StatusNotFound)
	if !strings.Contains(rec.Body.String(), `"session_not_found"`) {
		t.Fatalf("no code: %s", rec.Body.String())
	}
	expect(t, owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)

	alice2 := e.loginAs("alice@example.com", otherPassword, firefoxMac)
	expect(t, alice2.do(http.MethodDelete, "/api/me/sessions", nil), http.StatusOK)
	expect(t, owner.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	expect(t, alice.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)

	for _, x := range e.sessionsOf(alice2) {
		if x.ID == ownerList[0].ID {
			t.Fatal("alice sees the owner's session")
		}
	}
}

func TestSessionRoutesNeedALoginAndRefuseTokens(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	expect(t, e.client().do(http.MethodGet, "/api/me/sessions", nil), http.StatusUnauthorized)
	expect(t, e.client().do(http.MethodDelete, "/api/me/sessions", nil), http.StatusUnauthorized)
	expect(t, e.client().do(http.MethodDelete, "/api/me/sessions/1", nil), http.StatusUnauthorized)

	rec := owner.do(http.MethodPost, fmt.Sprintf("/api/workspaces/%d/tokens", ws), map[string]string{"name": "script"})
	expect(t, rec, http.StatusCreated)
	token := decode[struct{ Token string }](t, rec).Token
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/me/sessions"},
		{http.MethodDelete, "/api/me/sessions"},
		{http.MethodDelete, "/api/me/sessions/1"},
	} {
		c := e.client()
		r := c.req(tc.method, tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		expect(t, c.send(r), http.StatusForbidden)
	}
	if n := len(e.sessionsOf(owner)); n != 1 {
		t.Fatalf("sessions = %d", n)
	}
}

func TestRevokingASessionEndsItsPushSubscriptions(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	laptop := e.loginAs("owner@example.com", ownerPassword, firefoxMac)
	var public string
	for _, x := range e.sessionsOf(laptop) {
		if x.Current {
			public = x.ID
		}
	}
	var id int64
	if err := e.db.QueryRow(`SELECT id FROM sessions WHERE public_id = ?`, public).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO push_subscriptions (user_id, session_id, endpoint, p256dh, auth_sealed)
		VALUES ((SELECT user_id FROM sessions WHERE id = ?), ?, 'https://push.example/x', x'01', x'02')`, id, id); err != nil {
		t.Fatal(err)
	}
	expect(t, owner.do(http.MethodDelete, "/api/me/sessions/"+public, nil), http.StatusNoContent)
	if n := e.count(`SELECT count(*) FROM push_subscriptions WHERE session_id = ?`, id); n != 0 {
		t.Fatalf("%d subscriptions of a revoked session remain", n)
	}
}

func TestSessionRevokesAreAudited(t *testing.T) {
	e := newEnv(t, Options{})
	owner, ws := e.setup()
	ownerID := e.userID(owner)
	alice, _ := e.join(e.invite(owner, ws, "alice@example.com", RoleMember), "Alice")
	phone := e.loginAs("owner@example.com", ownerPassword, safariIOS)
	laptop := e.loginAs("owner@example.com", ownerPassword, firefoxMac)

	var phoneID, lapID string
	for _, x := range e.sessionsOf(laptop) {
		if x.Browser == "Safari" {
			phoneID = x.ID
		}
		if x.Current {
			lapID = x.ID
		}
	}
	aliceSession := e.sessionsOf(alice)[0].ID
	expect(t, laptop.do(http.MethodDelete, fmt.Sprintf("/api/me/sessions/%s", aliceSession), nil), http.StatusNotFound)
	expect(t, laptop.do(http.MethodDelete, fmt.Sprintf("/api/me/sessions/%s", lapID), nil), http.StatusBadRequest)
	expect(t, laptop.do(http.MethodDelete, "/api/me/sessions/"+strings.Repeat("0", 32), nil), http.StatusNotFound)
	expect(t, alice.do(http.MethodDelete, "/api/me/sessions", nil), http.StatusOK)
	if n := len(e.events(audit.SessionRevoked)) + len(e.events(audit.OtherSessionsRevoked)); n != 0 {
		t.Fatalf("%d events for refusals and an empty sweep", n)
	}

	expect(t, laptop.do(http.MethodDelete, fmt.Sprintf("/api/me/sessions/%s", phoneID), nil), http.StatusNoContent)
	wantEvent(t, e.one(audit.SessionRevoked), 0, ownerID, audit.TargetUser, ownerID)
	_ = phone

	expect(t, laptop.do(http.MethodDelete, "/api/me/sessions", nil), http.StatusOK)
	ev := e.one(audit.OtherSessionsRevoked)
	wantEvent(t, ev, 0, ownerID, audit.TargetUser, ownerID)
	if fmt.Sprint(ev.Details["logins_ended"]) != "1" {
		t.Fatalf("details = %v, want the 1 other login (the setup browser)", ev.Details)
	}
}

func TestRevokeOthersWithAGoneCallerEndsNothing(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	stale := e.loginAs("owner@example.com", ownerPassword, safariIOS)
	replacement := e.loginAs("owner@example.com", ownerPassword, firefoxMac)
	userID := e.userID(replacement)
	before := e.count(`SELECT count(*) FROM sessions WHERE user_id = ?`, userID)

	if _, err := e.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(stale.cookie.Value)); err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.revokeOtherSessions(context.Background(), userID, stale.cookie.Value)
	if !errors.Is(err, errSessionGone) || n != 0 {
		t.Fatalf("revokeOtherSessions = %d, %v; want errSessionGone", n, err)
	}
	if got := e.count(`SELECT count(*) FROM sessions WHERE user_id = ?`, userID); got != before-1 {
		t.Fatalf("sessions = %d, want %d: the replacement and the rest must stay", got, before-1)
	}
	expect(t, replacement.do(http.MethodGet, "/api/me", nil), http.StatusOK)
	if n := len(e.events(audit.OtherSessionsRevoked)); n != 0 {
		t.Fatalf("%d audit events for a refused sweep", n)
	}

	e.advance(60 * 24 * time.Hour)
	if _, err := e.svc.revokeOtherSessions(context.Background(), userID, replacement.cookie.Value); !errors.Is(err, errSessionGone) {
		t.Fatalf("an expired caller: %v", err)
	}
	if err := e.svc.revokeSession(context.Background(), userID, strings.Repeat("a", 32), replacement.cookie.Value); !errors.Is(err, errSessionGone) {
		t.Fatalf("revokeSession by an expired caller: %v", err)
	}
	if _, err := e.svc.revokeOtherSessions(context.Background(), userID, ""); !errors.Is(err, errSessionGone) {
		t.Fatalf("no token: %v", err)
	}
}

func TestStaleSessionIDNeverEndsALaterLogin(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	laptop := e.loginAs("owner@example.com", ownerPassword, firefoxMac)
	phone := e.loginAs("owner@example.com", ownerPassword, safariIOS)
	var phoneID string
	for _, x := range e.sessionsOf(laptop) {
		if x.Browser == "Safari" {
			phoneID = x.ID
		}
	}
	var phoneRow int64
	if err := e.db.QueryRow(`SELECT id FROM sessions WHERE public_id = ?`, phoneID).Scan(&phoneRow); err != nil {
		t.Fatal(err)
	}
	expect(t, laptop.do(http.MethodDelete, "/api/me/sessions/"+phoneID, nil), http.StatusNoContent)
	expect(t, phone.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)

	tablet := e.loginAs("owner@example.com", ownerPassword, firefoxMac)
	var tabletRow int64
	var tabletID string
	if err := e.db.QueryRow(`SELECT id, public_id FROM sessions WHERE token_hash = ?`, hashToken(tablet.cookie.Value)).Scan(&tabletRow, &tabletID); err != nil {
		t.Fatal(err)
	}
	if tabletRow != phoneRow {
		t.Logf("the row ID wasn't reused (%d, %d): the test no longer shows the hazard", phoneRow, tabletRow)
	}
	if tabletID == phoneID {
		t.Fatal("a new session got the public ID of an ended one")
	}

	expect(t, laptop.do(http.MethodDelete, "/api/me/sessions/"+phoneID, nil), http.StatusNotFound)
	expect(t, tablet.do(http.MethodGet, "/api/me", nil), http.StatusOK)

	seen := map[string]bool{}
	for _, x := range e.sessionsOf(laptop) {
		if !validSessionPublicID(x.ID) || seen[x.ID] {
			t.Fatalf("bad or repeated session ID %q", x.ID)
		}
		seen[x.ID] = true
	}
	expect(t, laptop.do(http.MethodDelete, fmt.Sprintf("/api/me/sessions/%d", tabletRow), nil), http.StatusNotFound)
	expect(t, tablet.do(http.MethodGet, "/api/me", nil), http.StatusOK)
}

func TestWatchSessionEndsWithTheLogin(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	laptop := e.loginAs("owner@example.com", ownerPassword, firefoxMac)
	phone := e.loginAs("owner@example.com", ownerPassword, safariIOS)
	other := e.loginAs("owner@example.com", ownerPassword, firefoxMac)

	watch := func(c *client, every time.Duration) (context.Context, context.CancelFunc) {
		t.Helper()
		ctx, stop := e.svc.WatchSession(c.req(http.MethodGet, "/api/me", nil), every)
		if ctx.Err() != nil {
			t.Fatal("the watch of a live session is already over")
		}
		t.Cleanup(stop)
		return ctx, stop
	}
	ended := func(ctx context.Context) bool {
		select {
		case <-ctx.Done():
			return true
		case <-time.After(3 * time.Second):
			return false
		}
	}
	alive := func(ctx context.Context) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
			return true
		}
	}

	phoneCtx, _ := watch(phone, time.Hour)
	laptopCtx, _ := watch(laptop, time.Hour)
	var phoneID string
	for _, x := range e.sessionsOf(laptop) {
		if x.Browser == "Safari" {
			phoneID = x.ID
		}
	}
	expect(t, laptop.do(http.MethodDelete, "/api/me/sessions/"+phoneID, nil), http.StatusNoContent)
	if !ended(phoneCtx) || !alive(laptopCtx) {
		t.Fatal("revoking one session must end its connections only")
	}

	otherCtx, _ := watch(other, time.Hour)
	expect(t, laptop.do(http.MethodDelete, "/api/me/sessions", nil), http.StatusOK)
	if !ended(otherCtx) || !alive(laptopCtx) {
		t.Fatal("signing out the others must end their connections and not the caller's")
	}

	ctx, _ := watch(laptop, 20*time.Millisecond)
	if _, err := e.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(laptop.cookie.Value)); err != nil {
		t.Fatal(err)
	}
	if !ended(ctx) {
		t.Fatal("a login deleted without a signal kept its connection")
	}

	again := e.loginAs("owner@example.com", ownerPassword, firefoxMac)
	againCtx, _ := watch(again, time.Hour)
	expect(t, again.do(http.MethodPost, "/api/logout", nil), http.StatusNoContent)
	if !ended(againCtx) {
		t.Fatal("logging out kept the connection")
	}
	one := e.loginAs("owner@example.com", ownerPassword, firefoxMac)
	two := e.loginAs("owner@example.com", ownerPassword, safariIOS)
	oneCtx, _ := watch(one, time.Hour)
	expect(t, two.do(http.MethodPost, "/api/me/password", map[string]string{"current_password": ownerPassword, "new_password": "a-brand-new-password"}), http.StatusNoContent)
	if !ended(oneCtx) {
		t.Fatal("a password change kept another login's connection")
	}

	ctx, stop := e.svc.WatchSession(e.client().req(http.MethodGet, "/api/me", nil), time.Hour)
	defer stop()
	if ctx.Err() == nil {
		t.Fatal("a request without a login got a live watch")
	}
}

func TestWatchSessionEndedBeforeItIsRegistered(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	laptop := e.loginAs("owner@example.com", ownerPassword, firefoxMac)
	phone := e.loginAs("owner@example.com", ownerPassword, safariIOS)

	var phoneID string
	for _, x := range e.sessionsOf(laptop) {
		if x.Browser == "Safari" {
			phoneID = x.ID
		}
	}
	for name, end := range map[string]func(){
		"signed out": func() { expect(t, phone.do(http.MethodPost, "/api/logout", nil), http.StatusNoContent) },
		"revoked": func() {
			expect(t, laptop.do(http.MethodDelete, "/api/me/sessions/"+phoneID, nil), http.StatusNoContent)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e.svc.hook = func(point string) {
				if point == "watch-session-looked-up" {
					e.svc.hook = nil
					end()
				}
			}
			t.Cleanup(func() { e.svc.hook = nil })
			ctx, stop := e.svc.WatchSession(phone.req(http.MethodGet, "/api/me", nil), time.Hour)
			defer stop()
			if ctx.Err() == nil {
				t.Fatal("a login ended before its watch was registered kept the connection")
			}
		})
		phone = e.loginAs("owner@example.com", ownerPassword, safariIOS)
		for _, x := range e.sessionsOf(laptop) {
			if x.Browser == "Safari" {
				phoneID = x.ID
			}
		}
	}
}

func TestWatchSessionRefusedWhenItsVerificationFails(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	phone := e.loginAs("owner@example.com", ownerPassword, safariIOS)

	e.svc.hook = func(point string) {
		if point == "watch-session-looked-up" {
			e.svc.hook = nil
			if _, err := e.svc.db.Exec(`ALTER TABLE sessions RENAME TO sessions_gone`); err != nil {
				t.Errorf("breaking the sessions lookup: %v", err)
			}
		}
	}
	t.Cleanup(func() { e.svc.hook = nil })
	ctx, stop := e.svc.WatchSession(phone.req(http.MethodGet, "/api/me", nil), time.Hour)
	defer stop()
	if ctx.Err() == nil {
		t.Fatal("a watch whose login could not be verified was admitted")
	}
}

func TestSessionAfterTheSecondStepRecordsItsDevice(t *testing.T) {
	e := newEnv(t, Options{})
	owner, _ := e.setup()
	two := e.turnOn(owner, ownerPassword)

	c := e.client()
	r := c.req(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": ownerPassword})
	r.Header.Set("User-Agent", firefoxMac)
	rec := c.send(r)
	expect(t, rec, http.StatusOK)
	challenge := decode[challengeReply](t, rec).Challenge
	r = c.req(http.MethodPost, "/api/login/two-factor", map[string]string{"challenge": challenge, "code": e.codeNow(two.secret)})
	r.Header.Set("User-Agent", safariIOS)
	expect(t, c.send(r), http.StatusOK)

	var found bool
	for _, x := range e.sessionsOf(c) {
		if x.Current {
			found = true
			if x.Browser != "Safari" || x.OS != "iOS" || !validSessionPublicID(x.ID) || x.LastSeenAt == "" {
				t.Fatalf("the session made at the second step: %+v", x)
			}
		}
	}
	if !found {
		t.Fatal("the session made at the second step is not listed as the current one")
	}
}

func TestMeNamesItsSession(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	first := e.loginAs("owner@example.com", ownerPassword, firefoxMac)
	current := func(c *client) string {
		for _, s := range e.sessionsOf(c) {
			if s.Current {
				return s.ID
			}
		}
		t.Fatal("no current session")
		return ""
	}
	rec := first.do(http.MethodGet, "/api/me", nil)
	expect(t, rec, http.StatusOK)
	if got, want := decode[Me](t, rec).Session, current(first); got == "" || got != want {
		t.Fatalf("/api/me session = %q, want %q", got, want)
	}
	before := decode[Me](t, rec).Session
	again := first.do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": ownerPassword})
	expect(t, again, http.StatusOK)
	if got := decode[Me](t, again).Session; got == "" || got == before || got != current(first) {
		t.Fatalf("login reply session = %q; the one before was %q, the current one is %q", got, before, current(first))
	}
}

func TestLogoutRevokesOnlyTheLoginItNames(t *testing.T) {
	e := newEnv(t, Options{})
	e.setup()
	first := e.loginAs("owner@example.com", ownerPassword, firefoxMac)
	meOf := func(c *client) Me {
		rec := c.do(http.MethodGet, "/api/me", nil)
		expect(t, rec, http.StatusOK)
		return decode[Me](t, rec)
	}
	original := meOf(first).Session
	expect(t, first.do(http.MethodPost, "/api/login", map[string]string{"email": "owner@example.com", "password": ownerPassword}), http.StatusOK)
	newer := meOf(first).Session
	if newer == original {
		t.Fatal("signing in again kept the same session")
	}
	rec := first.do(http.MethodPost, "/api/logout", map[string]string{"session": original})
	expect(t, rec, http.StatusConflict)
	if got := meOf(first).Session; got != newer {
		t.Fatalf("the newer login is %q after a refused sign-out, want %q", got, newer)
	}
	expect(t, first.do(http.MethodPost, "/api/logout", map[string]string{"session": newer}), http.StatusNoContent)
	expect(t, first.do(http.MethodGet, "/api/me", nil), http.StatusUnauthorized)
	expect(t, first.do(http.MethodPost, "/api/logout", map[string]string{"session": newer}), http.StatusNoContent)
	expect(t, first.do(http.MethodPost, "/api/logout", map[string]string{"session": original}), http.StatusNoContent)
}
