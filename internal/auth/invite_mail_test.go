package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/audit/audittest"
	"taskto.ai/taskto/internal/mail"
)

type emailedInvite struct {
	Token     string `json:"token"`
	EmailSent *bool  `json:"email_sent"`
}

func invitePath(ws int64) string { return fmt.Sprintf("/api/workspaces/%d/invites", ws) }

func TestInviteIsEmailedWhenAsked(t *testing.T) {
	e, sent := mailEnv(t, nil)
	logs := captureLogs(t)
	owner, ws := e.setup()

	rec := owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "Ana@Example.com", "role": "member", "send_email": true})
	expect(t, rec, http.StatusCreated)
	inv := decode[emailedInvite](t, rec)
	if inv.EmailSent == nil || !*inv.EmailSent || inv.Token == "" {
		t.Fatalf("reply = %s", rec.Body.String())
	}
	msgs := sent.messages()
	if len(msgs) != 1 || len(msgs[0].To) != 1 || msgs[0].To[0] != "ana@example.com" {
		t.Fatalf("messages = %+v", msgs)
	}
	if !strings.Contains(msgs[0].Subject, "Owner invited you to Home") || !strings.Contains(msgs[0].Text, "7 days") {
		t.Fatalf("message = %+v", msgs[0])
	}
	// The emailed link is the very invite the admin was given, and works.
	if got := tokenIn(t, msgs[0]); got != inv.Token {
		t.Fatalf("emailed token %q isn't the invite's", got)
	}
	if !strings.Contains(msgs[0].Text, publicURL+"/invite#") {
		t.Fatalf("text = %q", msgs[0].Text)
	}
	if strings.Contains(logs.String(), inv.Token) {
		t.Fatalf("the log has the invite token:\n%s", logs.String())
	}
	e.join(inv.Token, "Ana")
}

func TestInviteWithoutAskingSendsNothing(t *testing.T) {
	e, sent := mailEnv(t, nil)
	owner, ws := e.setup()
	for _, body := range []map[string]any{
		{"email": "ana@example.com"},
		{"email": "bea@example.com", "send_email": false},
	} {
		rec := owner.do(http.MethodPost, invitePath(ws), body)
		expect(t, rec, http.StatusCreated)
		if strings.Contains(rec.Body.String(), "email_sent") || decode[emailedInvite](t, rec).Token == "" {
			t.Fatalf("reply = %s", rec.Body.String())
		}
	}
	if n := len(sent.messages()); n != 0 {
		t.Fatalf("%d messages", n)
	}
}

func TestInviteEmailIsRefusedWhereTheServerCantEmailLinks(t *testing.T) {
	for name, opts := range map[string]Options{
		"no mail":       {PublicURL: publicURL},
		"no public url": {Mail: mail.NewService(&recorder{}, mail.Info{Host: "smtp.example.com"})},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, opts)
			owner, ws := e.setup()
			rec := owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "ana@example.com", "send_email": true})
			expect(t, rec, http.StatusConflict)
			if !strings.Contains(rec.Body.String(), `"mail_not_configured"`) {
				t.Fatalf("body = %s", rec.Body.String())
			}
			if n := e.count(`SELECT COUNT(*) FROM invites`); n != 0 {
				t.Fatalf("%d invites made for a refused request", n)
			}
			// Without asking, the flow is what it was.
			expect(t, owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "ana@example.com"}), http.StatusCreated)
		})
	}
}

func TestInviteStaysWhenTheEmailFails(t *testing.T) {
	e, sent := mailEnv(t, nil)
	logs := captureLogs(t)
	owner, ws := e.setup()
	sent.err = &mail.SendError{Stage: mail.StageRejected, Code: 550, Err: errors.New("550 relay.internal.example refused ana@example.com")}
	rec := owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "ana@example.com", "send_email": true})
	expect(t, rec, http.StatusCreated)
	inv := decode[emailedInvite](t, rec)
	if inv.EmailSent == nil || *inv.EmailSent || inv.Token == "" {
		t.Fatalf("reply = %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "relay.internal.example") {
		t.Fatalf("the answer has the relay's words: %s", rec.Body.String())
	}
	out := logs.String()
	if !strings.Contains(out, "the invite email wasn't sent") || !strings.Contains(out, "smtp_code=550") ||
		strings.Contains(out, "relay.internal.example") || strings.Contains(out, "ana@example.com") || strings.Contains(out, inv.Token) {
		t.Fatalf("log:\n%s", out)
	}
	// The link still works: the admin copies it.
	e.join(inv.Token, "Ana")
}

func TestInviteEmailLimitPerInvitedAddress(t *testing.T) {
	e, sent := mailEnv(t, nil)
	owner, ws := e.setup()
	for i := range inviteRecipientAttempts {
		rec := owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "ana@example.com", "send_email": true})
		if rec.Code != http.StatusCreated {
			t.Fatalf("invite %d = %d %s", i, rec.Code, rec.Body.String())
		}
	}
	before := e.count(`SELECT COUNT(*) FROM invites`)
	rec := owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "ana@example.com", "send_email": true})
	expect(t, rec, http.StatusTooManyRequests)
	if rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), `"too_many_invite_emails"`) {
		t.Fatalf("headers %v body %s", rec.Header(), rec.Body.String())
	}
	if e.count(`SELECT COUNT(*) FROM invites`) != before || len(sent.messages()) != inviteRecipientAttempts {
		t.Fatal("a limited request made an invite or sent mail")
	}
	// Copying the link has no such limit, and another address is free.
	expect(t, owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "ana@example.com"}), http.StatusCreated)
	expect(t, owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "bea@example.com", "send_email": true}), http.StatusCreated)
	e.advance(inviteWindow + time.Second)
	expect(t, owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "ana@example.com", "send_email": true}), http.StatusCreated)
}

func TestInviteEmailLimitPerAdmin(t *testing.T) {
	e, sent := mailEnv(t, nil)
	owner, ws := e.setup()
	for i := range inviteAdminAttempts {
		rec := owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": fmt.Sprintf("p%d@example.com", i), "send_email": true})
		if rec.Code != http.StatusCreated {
			t.Fatalf("invite %d = %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "one-more@example.com", "send_email": true})
	expect(t, rec, http.StatusTooManyRequests)
	if len(sent.messages()) != inviteAdminAttempts {
		t.Fatalf("%d messages", len(sent.messages()))
	}
	// The limit is the admin's, not the workspace's: another admin writes freely.
	admin, _ := e.join(e.invite(owner, ws, "admin@example.com", RoleAdmin), "Second")
	expect(t, admin.do(http.MethodPost, invitePath(ws), map[string]any{"email": "one-more@example.com", "send_email": true}), http.StatusCreated)
	// And the refused request wasn't counted against the address.
	expect(t, owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "ana@example.com"}), http.StatusCreated)
}

func TestInviteThatFailsGivesBackItsEmailAttempt(t *testing.T) {
	e, sent := mailEnv(t, nil)
	owner, ws := e.setup()
	// An existing member can't be invited: 409, and nothing sent or counted.
	for range inviteRecipientAttempts + 2 {
		rec := owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "owner@example.com", "send_email": true})
		expect(t, rec, http.StatusConflict)
	}
	if len(sent.messages()) != 0 {
		t.Fatal("mail was sent for an invite that wasn't made")
	}
	expect(t, owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "owner@example.com", "send_email": false}), http.StatusConflict)
}

func TestOnlyAnAdminOfTheWorkspaceEmailsItsInvites(t *testing.T) {
	e, sent := mailEnv(t, nil)
	owner, ws := e.setup()
	member, _ := e.join(e.invite(owner, ws, "member@example.com", RoleMember), "Maria")
	other := e.addWorkspace(e.userID(owner), "Other")

	rec := member.do(http.MethodPost, invitePath(ws), map[string]any{"email": "ana@example.com", "send_email": true})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member = %d", rec.Code)
	}
	// A stranger who isn't in the workspace at all.
	stranger := e.client()
	if rec := stranger.do(http.MethodPost, invitePath(ws), map[string]any{"email": "ana@example.com", "send_email": true}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", rec.Code)
	}
	// A member of one workspace can't use the other's name or admin.
	if rec := member.do(http.MethodPost, invitePath(other), map[string]any{"email": "ana@example.com", "send_email": true}); rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
		t.Fatalf("member of another workspace = %d", rec.Code)
	}
	if len(sent.messages()) != 0 {
		t.Fatalf("mail sent for refused requests: %+v", sent.messages())
	}
	// The message names the workspace it is for.
	expect(t, owner.do(http.MethodPost, invitePath(other), map[string]any{"email": "ana@example.com", "send_email": true}), http.StatusCreated)
	if m := sent.messages(); len(m) != 1 || !strings.Contains(m[0].Subject, "to Other") {
		t.Fatalf("messages = %+v", m)
	}
}

func TestInviteEmailIsInTheRightLanguage(t *testing.T) {
	e, sent := mailEnv(t, nil)
	owner, ws := e.setup()
	send := func(email string) mail.Message {
		t.Helper()
		expect(t, owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": email, "send_email": true}), http.StatusCreated)
		m := sent.messages()
		return m[len(m)-1]
	}
	// Nothing picked anywhere: English.
	if m := send("a@example.com"); !strings.Contains(m.Subject, "invited you to Home") {
		t.Fatalf("english: %q", m.Subject)
	}
	// The inviting admin's own language, for someone with no account.
	e.exec(t, `UPDATE users SET locale = 'pt-BR' WHERE email = 'owner@example.com'`)
	if m := send("b@example.com"); !strings.Contains(m.Subject, "convidou você para Home") {
		t.Fatalf("admin's: %q", m.Subject)
	}
	// The workspace's default beats the admin's.
	e.exec(t, `UPDATE workspaces SET locale = 'en' WHERE id = ?`, ws)
	if m := send("c@example.com"); !strings.Contains(m.Subject, "invited you to Home") {
		t.Fatalf("workspace's: %q", m.Subject)
	}
	// A person with an account who picked a language gets it.
	e.join(e.invite(owner, ws, "d@example.com", RoleMember), "Dora")
	e.exec(t, `UPDATE users SET locale = 'pt-BR' WHERE email = 'd@example.com'`)
	e.exec(t, `DELETE FROM memberships WHERE user_id = (SELECT id FROM users WHERE email = 'd@example.com')`)
	if m := send("d@example.com"); !strings.Contains(m.Subject, "convidou você para Home") {
		t.Fatalf("invitee's: %q", m.Subject)
	}
	// A person who picked none isn't given the default of another workspace they belong to: the
	// inviting workspace's default (English, here) applies.
	e.join(e.invite(owner, ws, "f@example.com", RoleMember), "Fay")
	other := e.addWorkspace(e.userID(owner), "Elsewhere")
	e.exec(t, `UPDATE workspaces SET locale = 'pt-BR' WHERE id = ?`, other)
	e.exec(t, `INSERT INTO memberships (workspace_id, user_id, role) VALUES (?, (SELECT id FROM users WHERE email = 'f@example.com'), 'member')`, other)
	e.exec(t, `DELETE FROM memberships WHERE workspace_id = ? AND user_id = (SELECT id FROM users WHERE email = 'f@example.com')`, ws)
	if m := send("f@example.com"); !strings.Contains(m.Subject, "invited you to Home") {
		t.Fatalf("an unrelated workspace's default: %q", m.Subject)
	}
}

func (e *env) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := e.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestAnEmailedInviteIsAuditedOnlyWhenTheRelayTookIt(t *testing.T) {
	e, sent := mailEnv(t, nil)
	owner, ws := e.setup()
	ownerID := e.userID(owner)

	rec := owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "ana@example.com", "send_email": true})
	expect(t, rec, http.StatusCreated)
	inv := decode[emailedInvite](t, rec)
	row := e.one(audit.InviteEmailed)
	wantEvent(t, row, ws, ownerID, audit.TargetInvite, e.scalar(`SELECT id FROM invites WHERE email = 'ana@example.com'`))
	audittest.NoSecrets(t, e.db, map[string]string{"the invite token": inv.Token})

	// Not asked for, or refused by the relay: no such event, though the invite is made either way.
	expect(t, owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "bea@example.com"}), http.StatusCreated)
	sent.err = errors.New("the relay is down")
	expect(t, owner.do(http.MethodPost, invitePath(ws), map[string]any{"email": "cai@example.com", "send_email": true}), http.StatusCreated)
	if n := len(e.events(audit.InviteEmailed)); n != 1 {
		t.Fatalf("%d invite_emailed events, want 1", n)
	}
	if n := len(e.events(audit.InviteCreated)); n != 3 {
		t.Fatalf("%d invite_created events, want 3", n)
	}
}

func (e *env) scalar(query string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.db.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// slowRelay is a mail.Sender that takes a while, as a slow relay does, and gives up when the
// context it is sent with is cancelled, as a real SMTP dial or write does.
type slowRelay struct {
	recorder
	started chan struct{}
	release chan struct{}
}

func (s *slowRelay) Send(ctx context.Context, m mail.Message) error {
	close(s.started)
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.recorder.Send(ctx, m)
}

func TestInviteEmailSurvivesTheClientDisconnecting(t *testing.T) {
	relay := &slowRelay{started: make(chan struct{}), release: make(chan struct{})}
	e := newEnv(t, Options{
		PublicURL: publicURL,
		Mail:      mail.NewService(relay, mail.Info{Host: "smtp.example.com", Port: 587, Security: "starttls", From: "taskto@example.com"}),
	})
	owner, ws := e.setup()
	var open sync.Once
	release := func() { open.Do(func() { close(relay.release) }) }
	t.Cleanup(release)

	ctx, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	req := owner.req(http.MethodPost, invitePath(ws), map[string]any{"email": "ana@example.com", "send_email": true}).WithContext(ctx)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- owner.send(req) }()

	select {
	case <-relay.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay was never reached")
	}
	// The client goes away while the relay is still working, then the relay finishes.
	disconnect()
	release()

	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never answered")
	}
	expect(t, rec, http.StatusCreated)
	if msgs := relay.messages(); len(msgs) != 1 || msgs[0].To[0] != "ana@example.com" {
		t.Fatalf("the email wasn't delivered after the disconnect: %+v", msgs)
	}
	if got := decode[emailedInvite](t, rec); got.EmailSent == nil || !*got.EmailSent {
		t.Fatalf("reply = %s", rec.Body.String())
	}
	if n := len(e.events(audit.InviteEmailed)); n != 1 {
		t.Fatalf("%d invite_emailed events, want 1", n)
	}
}
