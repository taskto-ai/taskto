package server

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/mail"
)

const testPublicURL = "https://taskto.example.com"

// resetPassword is the fake password a test sets with a reset link.
const resetPassword = "fake-brand-new-password" //nolint:gosec // G101: a fake test value

// linkStack is a stack whose server sends mail through rec and knows its public address.
func linkStack(t *testing.T, rec *recorder, publicURL string) *stack {
	t.Helper()
	return newStackWith(t, func(o *Options) {
		o.Mail = mail.NewService(rec, testInfo)
		o.MailTestCooldown = -1
		o.Auth.PublicURL = publicURL
	})
}

// waitForMail waits until rec holds n messages.
func waitForMail(t *testing.T, rec *recorder, n int) []mail.Message {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := rec.messages(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waited for %d messages, have %d", n, len(rec.messages()))
	return nil
}

var resetLinkRe = regexp.MustCompile(regexp.QuoteMeta(testPublicURL) + `/reset-password#([A-Za-z0-9_-]+)`)

func TestForgottenPasswordIsResetFromTheLoginPageAlone(t *testing.T) {
	rec := &recorder{}
	s := linkStack(t, rec, testPublicURL)

	code, methods := s.browser("", http.MethodGet, "/api/login/methods", nil)
	if code != http.StatusOK || methods["reset_by_email"] != true {
		t.Fatalf("methods = %d %v", code, methods)
	}
	// No login, no cookie: only the address.
	code, body := s.browser("", http.MethodPost, "/api/password-reset/request", map[string]any{"email": "bob@example.com"})
	if code != http.StatusAccepted || len(body) != 0 {
		t.Fatalf("request = %d %v", code, body)
	}
	msgs := waitForMail(t, rec, 1)
	if len(msgs[0].To) != 1 || msgs[0].To[0] != "bob@example.com" || !strings.Contains(msgs[0].Subject, "password") || msgs[0].HTML == "" {
		t.Fatalf("message = %+v", msgs[0])
	}
	found := resetLinkRe.FindStringSubmatch(msgs[0].Text)
	if found == nil {
		t.Fatalf("no link in:\n%s", msgs[0].Text)
	}
	code, body = s.browser("", http.MethodPost, "/api/password-reset", map[string]any{"token": found[1], "password": resetPassword})
	if code != http.StatusOK {
		t.Fatalf("use the link = %d %v", code, body)
	}
	if code, _ := s.browser("", http.MethodPost, "/api/login", map[string]any{"email": "bob@example.com", "password": resetPassword}); code != http.StatusOK {
		t.Fatalf("login with the new password = %d", code)
	}
	// An address no account has gets the same answer, and no message.
	code, body = s.browser("", http.MethodPost, "/api/password-reset/request", map[string]any{"email": "nobody@example.com"})
	if code != http.StatusAccepted || len(body) != 0 {
		t.Fatalf("unknown address = %d %v", code, body)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(rec.messages()); n != 1 {
		t.Fatalf("%d messages: an unknown address gets none", n)
	}
}

func TestResetByEmailIsOffUntilTheServerCanEmailLinks(t *testing.T) {
	for name, tc := range map[string]struct {
		rec       *recorder
		publicURL string
	}{
		"mail without a public address": {&recorder{}, ""},
		"no mail":                       {nil, testPublicURL},
	} {
		t.Run(name, func(t *testing.T) {
			var s *stack
			if tc.rec == nil {
				s = newStackWith(t, func(o *Options) { o.Auth.PublicURL = tc.publicURL })
			} else {
				s = linkStack(t, tc.rec, tc.publicURL)
			}
			code, methods := s.browser("", http.MethodGet, "/api/login/methods", nil)
			if code != http.StatusOK || methods["reset_by_email"] != false {
				t.Fatalf("methods = %d %v", code, methods)
			}
			code, body := s.browser("", http.MethodPost, "/api/password-reset/request", map[string]any{"email": "bob@example.com"})
			if code != http.StatusConflict || body["code"] != "mail_not_configured" {
				t.Fatalf("request = %d %v", code, body)
			}
			// What an admin sees: the server can't email links.
			code, body = s.browser(s.alice.cookie, http.MethodGet, mailPath(s.one, ""), nil)
			if code != http.StatusOK || body["sends_links"] != false {
				t.Fatalf("status = %d %v", code, body)
			}
		})
	}
}

func TestMailStatusSaysWhenLinksAreEmailed(t *testing.T) {
	s := linkStack(t, &recorder{}, testPublicURL)
	code, body := s.browser(s.alice.cookie, http.MethodGet, mailPath(s.one, ""), nil)
	if code != http.StatusOK || body["configured"] != true || body["sends_links"] != true {
		t.Fatalf("status = %d %v", code, body)
	}
}

func TestResetRequestIsLimitedPerEmailForEveryAddress(t *testing.T) {
	rec := &recorder{}
	s := linkStack(t, rec, testPublicURL)
	for _, email := range []string{"bob@example.com", "nobody@example.com"} {
		var limited bool
		for i := 0; i < 8; i++ {
			code, body := s.browser("", http.MethodPost, "/api/password-reset/request", map[string]any{"email": email})
			if code == http.StatusTooManyRequests {
				if body["code"] != "too_many_resets" {
					t.Fatalf("%s: body = %v", email, body)
				}
				if i != 5 {
					t.Fatalf("%s: limited at request %d, want 5 (after 5 allowed)", email, i)
				}
				limited = true
				break
			}
			if code != http.StatusAccepted {
				t.Fatalf("%s request %d = %d %v", email, i, code, body)
			}
			time.Sleep(20 * time.Millisecond) // the work behind each answer is bounded
		}
		if !limited {
			t.Fatalf("%s: never limited", email)
		}
	}
}

func TestResetEmailGoesOnlyToTheAddressAsked(t *testing.T) {
	// carol asks for bob's address while signed in: the link goes to bob, carol gets nothing.
	rec := &recorder{}
	s := linkStack(t, rec, testPublicURL)
	code, body := s.browser(s.carol.cookie, http.MethodPost, "/api/password-reset/request", map[string]any{"email": "bob@example.com"})
	if code != http.StatusAccepted || len(body) != 0 {
		t.Fatalf("request = %d %v", code, body)
	}
	msgs := waitForMail(t, rec, 1)
	if len(msgs) != 1 || msgs[0].To[0] != "bob@example.com" || strings.Contains(fmt.Sprint(msgs[0]), "carol@example.com") {
		t.Fatalf("messages = %+v", msgs)
	}
}
