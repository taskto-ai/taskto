package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/mail"
)

// recorder is a mail.Sender that keeps what it is given, or fails.
type recorder struct {
	mu   sync.Mutex
	sent []mail.Message
	err  error
}

func (r *recorder) Send(_ context.Context, m mail.Message) error {
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

var testInfo = mail.Info{Host: "smtp.example.com", Port: 587, Security: "starttls", From: `"taskto" <taskto@example.com>`, SignsIn: true}

func mailStack(t *testing.T, rec *recorder) *stack {
	t.Helper()
	return newStackWith(t, func(o *Options) { o.Mail = mail.NewService(rec, testInfo); o.MailTestCooldown = -1 })
}

// limitedStack is a stack with the test route's real limits (the default cooldown).
func limitedStack(t *testing.T, rec *recorder) *stack {
	t.Helper()
	return newStackWith(t, func(o *Options) { o.Mail = mail.NewService(rec, testInfo) })
}

func mailPath(ws int64, tail string) string {
	return fmt.Sprintf("/api/workspaces/%d/mail%s", ws, tail)
}

func TestMailStatusIsForAdminsAndNeverHoldsASecret(t *testing.T) {
	s := mailStack(t, &recorder{})
	code, body := s.browser(s.alice.cookie, http.MethodGet, mailPath(s.one, ""), nil)
	if code != http.StatusOK || body["configured"] != true || body["host"] != "smtp.example.com" || body["port"] != float64(587) ||
		body["security"] != "starttls" || body["password"] != "set" || !strings.Contains(fmt.Sprint(body["from"]), "taskto@example.com") {
		t.Fatalf("admin: %d %v", code, body)
	}
	for k := range body {
		switch k {
		case "configured", "sends_links", "host", "port", "security", "from", "password":
		default:
			t.Fatalf("the status has an unexpected field %q: %v", k, body)
		}
	}
	// The server's SMTP server is the whole server's: an admin of another workspace sees it too.
	if code, body := s.browser(s.alice.cookie, http.MethodGet, mailPath(s.two, ""), nil); code != http.StatusOK || body["host"] != "smtp.example.com" {
		t.Fatalf("admin of another workspace: %d %v", code, body)
	}
	// bob is a member of one, not an admin; he isn't in two at all.
	if code, body := s.browser(s.bob.cookie, http.MethodGet, mailPath(s.one, ""), nil); code != http.StatusForbidden || body["host"] != nil {
		t.Fatalf("member: %d %v", code, body)
	}
	if code, body := s.browser(s.bob.cookie, http.MethodGet, mailPath(s.two, ""), nil); code != http.StatusNotFound || body["host"] != nil {
		t.Fatalf("outsider: %d %v", code, body)
	}
	if code, _ := s.browser("", http.MethodGet, mailPath(s.one, ""), nil); code != http.StatusUnauthorized {
		t.Fatalf("no login: %d", code)
	}
	token, _ := s.newToken(s.alice.cookie, s.one, "monitor")
	if code, body := s.api(token, http.MethodGet, mailPath(s.one, ""), nil); code == http.StatusOK || body["host"] != nil {
		t.Fatalf("access token: %d %v", code, body)
	}
}

func TestMailNotConfigured(t *testing.T) {
	s := newStack(t)
	code, body := s.browser(s.alice.cookie, http.MethodGet, mailPath(s.one, ""), nil)
	if code != http.StatusOK || body["configured"] != false || body["host"] != nil || body["password"] != nil {
		t.Fatalf("status: %d %v", code, body)
	}
	code, body = s.browser(s.alice.cookie, http.MethodPost, mailPath(s.one, "/test"), map[string]any{})
	if code != http.StatusConflict || body["code"] != "mail_not_configured" {
		t.Fatalf("test: %d %v", code, body)
	}
}

func TestMailTestGoesToTheCallerOnly(t *testing.T) {
	rec := &recorder{}
	s := mailStack(t, rec)

	code, body := s.browser(s.alice.cookie, http.MethodPost, mailPath(s.one, "/test"), map[string]any{})
	if code != http.StatusOK || body["sent_to"] != "alice@example.com" {
		t.Fatalf("test: %d %v", code, body)
	}
	got := rec.messages()
	if len(got) != 1 || len(got[0].To) != 1 || got[0].To[0] != "alice@example.com" {
		t.Fatalf("sent %+v, want one message to alice only", got)
	}
	if !strings.Contains(got[0].Subject, "test email") || !strings.Contains(got[0].Text, "Alice") || got[0].HTML == "" {
		t.Fatalf("message = %+v", got[0])
	}

	// A recipient in the request is ignored: there is no way to name one.
	if code, body := s.browser(s.alice.cookie, http.MethodPost, mailPath(s.one, "/test"), map[string]any{"to": "victim@example.com"}); code != http.StatusOK && code != http.StatusBadRequest {
		t.Fatalf("with a recipient: %d %v", code, body)
	}
	for _, m := range rec.messages() {
		if strings.Contains(strings.Join(m.To, " "), "victim") {
			t.Fatalf("a message went to %v", m.To)
		}
	}

	// In the caller's language: the one they picked, else the workspace's default.
	n := len(rec.messages())
	if code, body := s.browser(s.alice.cookie, http.MethodPatch, "/api/me", map[string]any{"locale": "pt-BR"}); code != http.StatusOK {
		t.Fatalf("locale: %d %v", code, body)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodPost, mailPath(s.one, "/test"), map[string]any{}); code != http.StatusOK {
		t.Fatalf("test: %d %v", code, body)
	}
	if m := rec.messages()[n]; !strings.Contains(m.Subject, "teste") || !strings.Contains(m.HTML, `lang="pt-BR"`) {
		t.Fatalf("not in Portuguese: %+v", m)
	}
}

func TestMailTestIsForAdminsOfTheWorkspace(t *testing.T) {
	rec := &recorder{}
	s := mailStack(t, rec)
	for name, tt := range map[string]struct {
		cookie string
		ws     int64
		want   int
	}{
		"a member":                         {s.bob.cookie, s.one, http.StatusForbidden},
		"another member":                   {s.carol.cookie, s.one, http.StatusForbidden},
		"someone outside the workspace":    {s.bob.cookie, s.two, http.StatusNotFound},
		"nobody signed in":                 {"", s.one, http.StatusUnauthorized},
		"a workspace that doesn't exist":   {s.alice.cookie, 99999, http.StatusNotFound},
		"an admin, in their own workspace": {s.alice.cookie, s.two, http.StatusOK},
	} {
		code, body := s.browser(tt.cookie, http.MethodPost, mailPath(tt.ws, "/test"), map[string]any{})
		if code != tt.want {
			t.Errorf("%s: %d %v, want %d", name, code, body, tt.want)
		}
	}
	// Only the admin's own test went out, and to the admin.
	got := rec.messages()
	if len(got) != 1 || got[0].To[0] != "alice@example.com" {
		t.Fatalf("sent %+v", got)
	}
	// An access token never sends mail.
	token, _ := s.newToken(s.alice.cookie, s.one, "mail")
	if code, _ := s.api(token, http.MethodPost, mailPath(s.one, "/test"), map[string]any{}); code == http.StatusOK {
		t.Fatalf("an access token sent a message")
	}
	// The CSRF guard applies: a request from another site, or a body that isn't JSON, sends nothing.
	cross := s.request(http.MethodPost, mailPath(s.one, "/test"), map[string]any{}, func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Cookie", "taskto_session="+s.alice.cookie)
	})
	form := s.request(http.MethodPost, mailPath(s.one, "/test"), map[string]any{}, sameOrigin(s.alice.cookie), func(r *http.Request) {
		r.Header.Set("Content-Type", "text/plain")
	})
	if cross.Code != http.StatusForbidden || form.Code != http.StatusUnsupportedMediaType || len(rec.messages()) != 1 {
		t.Fatalf("cross-site %d, text %d, %d messages", cross.Code, form.Code, len(rec.messages()))
	}
}

func TestMailTestFailuresAreCodesNotServerText(t *testing.T) {
	const leak = "mx7.internal.corp.example 10.9.8.7"
	for name, tt := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"connect":  {&mail.SendError{Stage: mail.StageConnect, Err: errors.New("dial " + leak + ": refused")}, http.StatusBadGateway, "mail_connect_failed"},
		"tls":      {&mail.SendError{Stage: mail.StageTLS, Err: errors.New("x509 for " + leak)}, http.StatusBadGateway, "mail_tls_failed"},
		"auth":     {&mail.SendError{Stage: mail.StageAuth, Code: 535, Err: errors.New("535 " + leak)}, http.StatusBadGateway, "mail_auth_failed"},
		"rejected": {&mail.SendError{Stage: mail.StageRejected, Code: 550, Err: errors.New("550 " + leak)}, http.StatusBadGateway, "mail_rejected"},
		"other":    {&mail.SendError{Stage: mail.StageSend, Err: errors.New("EOF " + leak)}, http.StatusBadGateway, "mail_failed"},
		"egress": {&mail.SendError{Stage: mail.StageConnect, Err: &egress.RefusedError{Host: "smtp.example.com", Reason: "not on the allowlist"}},
			http.StatusBadGateway, "egress_refused"},
		"unexpected": {errors.New("something broke " + leak), http.StatusInternalServerError, "internal"},
	} {
		t.Run(name, func(t *testing.T) {
			s := mailStack(t, &recorder{err: tt.err})
			code, body := s.browser(s.alice.cookie, http.MethodPost, mailPath(s.one, "/test"), map[string]any{})
			if code != tt.status || body["code"] != tt.code {
				t.Fatalf("%d %v, want %d %s", code, body, tt.status, tt.code)
			}
			if strings.Contains(fmt.Sprint(body), "10.9.8.7") || strings.Contains(fmt.Sprint(body), "internal.corp") {
				t.Fatalf("the answer repeats the server's text: %v", body)
			}
		})
	}
	s := mailStack(t, &recorder{err: &mail.SendError{Stage: mail.StageRejected, Code: 550, Err: errors.New("no")}})
	_, body := s.browser(s.alice.cookie, http.MethodPost, mailPath(s.one, "/test"), map[string]any{})
	if p, _ := body["params"].(map[string]any); p["smtp_code"] != float64(550) {
		t.Fatalf("params = %v", body["params"])
	}
}

// A person sends one test email per cooldown, in whichever workspace; the answer is a 429 with
// a code the web app translates and how long to wait, and nothing more is sent.
func TestMailTestHasACooldownPerPersonAcrossWorkspaces(t *testing.T) {
	rec := &recorder{}
	s := limitedStack(t, rec)
	if code, body := s.browser(s.alice.cookie, http.MethodPost, mailPath(s.one, "/test"), map[string]any{}); code != http.StatusOK {
		t.Fatalf("first: %d %v", code, body)
	}
	for _, ws := range []int64{s.one, s.two} {
		r := s.request(http.MethodPost, mailPath(ws, "/test"), map[string]any{}, sameOrigin(s.alice.cookie))
		if r.Code != http.StatusTooManyRequests || !strings.Contains(r.Body.String(), `"mail_test_rate_limited"`) {
			t.Fatalf("workspace %d: %d %s", ws, r.Code, r.Body.String())
		}
		if secs, err := strconv.Atoi(r.Header().Get("Retry-After")); err != nil || secs < 1 || secs > 30 {
			t.Fatalf("Retry-After = %q", r.Header().Get("Retry-After"))
		}
	}
	if n := len(rec.messages()); n != 1 {
		t.Fatalf("%d messages sent, want 1", n)
	}
}

func TestMailTestCooldownIsForThePersonNotTheServer(t *testing.T) {
	rec := &recorder{}
	s := limitedStack(t, rec)
	// carol is made an admin; she and alice each send their own.
	s.exec(`UPDATE memberships SET role = 'admin' WHERE user_id = ? AND workspace_id = ?`, s.carol.id, s.one)
	if code, body := s.browser(s.alice.cookie, http.MethodPost, mailPath(s.one, "/test"), map[string]any{}); code != http.StatusOK {
		t.Fatalf("alice: %d %v", code, body)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodPost, mailPath(s.one, "/test"), map[string]any{}); code != http.StatusTooManyRequests {
		t.Fatalf("alice again: %d %v", code, body)
	}
	if code, body := s.browser(s.carol.cookie, http.MethodPost, mailPath(s.one, "/test"), map[string]any{}); code != http.StatusOK {
		t.Fatalf("another person was limited by alice's send: %d %v", code, body)
	}
}

// A failed send counts too: it is the relay that is being asked.
func TestMailTestCooldownCountsFailuresToo(t *testing.T) {
	rec := &recorder{err: &mail.SendError{Stage: mail.StageConnect, Err: errors.New("down")}}
	s := limitedStack(t, rec)
	if code, _ := s.browser(s.alice.cookie, http.MethodPost, mailPath(s.one, "/test"), map[string]any{}); code != http.StatusBadGateway {
		t.Fatalf("first: %d", code)
	}
	if code, body := s.browser(s.alice.cookie, http.MethodPost, mailPath(s.one, "/test"), map[string]any{}); code != http.StatusTooManyRequests {
		t.Fatalf("second: %d %v", code, body)
	}
}

func TestMailTestGate(t *testing.T) {
	now := time.Unix(1000, 0)
	g := newMailTestGate(30*time.Second, func() time.Time { return now })

	leave, ok, _ := g.enter(1)
	if !ok {
		t.Fatal("the first send was refused")
	}
	leave()
	leave() // ending twice frees one slot, not two
	if _, ok, wait := g.enter(1); ok || wait != 30*time.Second {
		t.Fatalf("the same person at once: ok %v, wait %v", ok, wait)
	}
	now = now.Add(10 * time.Second)
	if _, ok, wait := g.enter(1); ok || wait != 20*time.Second {
		t.Fatalf("after 10s: ok %v, wait %v (a refusal must not restart the cooldown)", ok, wait)
	}
	now = now.Add(20 * time.Second)
	if leave, ok, _ := g.enter(1); !ok {
		t.Fatal("after the cooldown the send was refused")
	} else {
		leave()
	}

	// At most mailTestConcurrency at once, for different people; a refusal for being busy
	// doesn't use the person's cooldown, and a finished send frees a place.
	g = newMailTestGate(time.Second, func() time.Time { return now })
	var leaves []func()
	for u := int64(1); u <= mailTestConcurrency; u++ {
		l, ok, _ := g.enter(u)
		if !ok {
			t.Fatalf("send %d refused", u)
		}
		leaves = append(leaves, l)
	}
	if _, ok, wait := g.enter(99); ok || wait <= 0 {
		t.Fatalf("beyond the cap: ok %v, wait %v", ok, wait)
	}
	leaves[0]()
	l, ok, _ := g.enter(99)
	if !ok {
		t.Fatal("a place was freed and 99 was still refused: the busy refusal used the cooldown")
	}
	l()

	// A negative cooldown is none.
	g = newMailTestGate(-1, func() time.Time { return now })
	for range 5 {
		l, ok, _ := g.enter(1)
		if !ok {
			t.Fatal("refused without a cooldown")
		}
		l()
	}
}

// The ones that ended their cooldown don't pile up in memory.
func TestMailTestGateForgetsEndedCooldowns(t *testing.T) {
	now := time.Unix(0, 0)
	g := newMailTestGate(time.Second, func() time.Time { return now })
	for u := int64(0); u < 50; u++ {
		l, ok, _ := g.enter(u)
		if !ok {
			t.Fatal("refused")
		}
		l()
		now = now.Add(2 * time.Second)
	}
	if len(g.last) > 2 {
		t.Fatalf("%d people remembered", len(g.last))
	}
}
