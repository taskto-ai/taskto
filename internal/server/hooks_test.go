package server

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/integrations"
	"taskto.ai/taskto/internal/integrations/testprovider"
	"taskto.ai/taskto/internal/serverkey"
)

const hooksOrigin = "https://taskto.example.test"

func hooksStack(t *testing.T) *stack {
	t.Helper()
	return newStackDB(t, func(o *Options, sqlDB *sql.DB) {
		key, err := serverkey.Load(o.BackupDataDir)
		if err != nil {
			t.Fatal(err)
		}
		o.Integrations = integrations.New(integrations.NewStore(sqlDB, key), integrations.Options{PublicURL: hooksOrigin})
	})
}

// makeIntegration creates a Stripe integration in workspace ws as alice and returns its id and
// its public id.
func (s *stack) makeIntegration(ws int64, name, webhookSecret string) (int64, string) {
	s.t.Helper()
	code, body := s.browser(s.alice.cookie, http.MethodPost, fmt.Sprintf("/api/workspaces/%d/integrations", ws), map[string]any{
		"provider": "stripe", "name": name, "secrets": map[string]string{"api_key": testprovider.TestAPIKey, "webhook_secret": webhookSecret},
		"max_amount_minor": 100000, "currencies": []string{"EUR"},
	})
	if code != http.StatusCreated {
		s.t.Fatalf("create integration = %d %v", code, body)
	}
	url, _ := body["webhook_url"].(string)
	if !strings.HasPrefix(url, hooksOrigin+integrations.HookPrefix) {
		s.t.Fatalf("webhook_url = %q", url)
	}
	return id(body["id"]), strings.TrimPrefix(url, hooksOrigin+integrations.HookPrefix)
}

func (s *stack) hook(publicID string, body []byte, secret string, at time.Time, mod func(*http.Request)) *httptest.ResponseRecorder {
	s.t.Helper()
	r := httptest.NewRequest(http.MethodPost, integrations.HookPrefix+publicID, bytes.NewReader(body))
	r.RemoteAddr = "192.0.2.7:5000"
	if secret != "" {
		r.Header.Set(testprovider.SignatureHeader, testprovider.Sign(secret, at, body))
	}
	if mod != nil {
		mod(r)
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, r)
	return rec
}

// The route works through the whole server, outside /api: the CSRF guard and the account checks
// don't touch it, a signature alone opens it, and nothing of workspaces is told by what it says.
func TestTheWebhookRouteThroughTheServer(t *testing.T) {
	s := hooksStack(t)
	oneID, one := s.makeIntegration(s.one, "shop", testprovider.TestWebhookSecret)
	twoSecret := "whsec_" + strings.Repeat("Twoo", 6)
	twoInt, two := s.makeIntegration(s.two, "shop", twoSecret)

	now := time.Now()
	ev := testprovider.NewEvent("checkout.session.expired", map[string]any{
		"id": "cs_test_abc123", "object": "checkout.session", "mode": "payment", "payment_status": "unpaid",
		"client_reference_id": strings.Repeat("0f", 16), "amount_total": 5000, "currency": "eur",
	}, now)
	body := ev.Body()
	count := func(integration int64) int64 {
		return s.scalar(`SELECT count(*) FROM integration_events WHERE integration_id = ?`, integration)
	}

	// An id that doesn't exist, and a signature that doesn't verify, are the same JSON 404: not the
	// web app's page, which any other unknown path gets.
	unknown := s.hook(strings.Repeat("a", 32), body, testprovider.TestWebhookSecret, now, nil)
	wrong := s.hook(one, body, testprovider.OtherWebhookSecret, now, nil)
	unsigned := s.hook(one, body, "", now, nil)
	for name, rec := range map[string]*httptest.ResponseRecorder{"unknown": unknown, "wrong signature": wrong, "unsigned": unsigned} {
		if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") != "application/json" || !strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Errorf("%s: %d %q %s", name, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
		if rec.Body.String() != unknown.Body.String() {
			t.Errorf("%s answered %q, an unknown id %q", name, rec.Body.String(), unknown.Body.String())
		}
	}

	// Workspace one's secret against workspace two's integration, and the other way: refused.
	if rec := s.hook(two, body, testprovider.TestWebhookSecret, now, nil); rec.Code != http.StatusNotFound {
		t.Errorf("workspace one's secret on workspace two's integration = %d", rec.Code)
	}
	if rec := s.hook(one, body, twoSecret, now, nil); rec.Code != http.StatusNotFound {
		t.Errorf("workspace two's secret on workspace one's integration = %d", rec.Code)
	}
	if count(oneID)+count(twoInt) != 0 {
		t.Fatal("a refused delivery stored an event")
	}

	// A signed delivery is accepted with no session, no origin headers and whatever cookie, and a
	// browser's session never stands in for the signature.
	for name, mod := range map[string]func(*http.Request){
		"no cookie":         nil,
		"another account's": func(r *http.Request) { r.Header.Set("Cookie", "taskto_session="+s.bob.cookie) },
		"a cross-site POST": func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set("Origin", "https://evil.example")
		},
		"the account header": func(r *http.Request) { r.Header.Set("X-Taskto-Expected-User", "999999") },
	} {
		if rec := s.hook(one, body, testprovider.TestWebhookSecret, now, mod); rec.Code != http.StatusOK || rec.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if rec := s.hook(one, body, "", now, func(r *http.Request) {
		r.Header.Set("Cookie", "taskto_session="+s.alice.cookie)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}); rec.Code != http.StatusNotFound {
		t.Errorf("an admin's session without a signature = %d", rec.Code)
	}
	if count(oneID) != 1 || count(twoInt) != 0 {
		t.Fatalf("events: %d and %d, want one for workspace one's integration and none for two's", count(oneID), count(twoInt))
	}
	if got := s.scalar(`SELECT count(*) FROM integration_events WHERE kind = 'payment.expired' AND state = 'pending'`); got != 1 {
		t.Errorf("the stored event is not a pending payment.expired: %d", got)
	}

	// Only POST is the route's: another method gets the app's own handling, and stores nothing.
	r := httptest.NewRequest(http.MethodGet, integrations.HookPrefix+one, nil)
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, r)
	if count(oneID) != 1 {
		t.Errorf("a GET stored an event (%d)", rec.Code)
	}
}
