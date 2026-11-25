package apps

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

// stubLinks stands in for the integrations' side of a payment link (package integrations tests the
// real one against a fake Stripe): it makes a link once per entry, as that side does, and counts
// what it was asked.
type stubLinks struct {
	mu    sync.Mutex
	calls []PaymentLinkRequest
	made  map[int64]PaymentLink
	fail  error
}

func (s *stubLinks) OpenPaymentLink(_ context.Context, req PaymentLinkRequest) (PaymentLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if s.fail != nil {
		return PaymentLink{}, s.fail
	}
	if l, ok := s.made[req.EffectID]; ok {
		return l, nil
	}
	if s.made == nil {
		s.made = map[int64]PaymentLink{}
	}
	n := len(s.made) + 1
	l := PaymentLink{URL: fmt.Sprintf("https://pay.example.test/c/%d", n), ObjectID: fmt.Sprintf("cs_test_%d", n), AmountMinor: 12010, Currency: "EUR"}
	s.made[req.EffectID] = l
	return l, nil
}

func (s *stubLinks) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// payFX is the invoicing app live, its slot approved and bound to an integration allowed in the
// space, with an invoice of 120.10 ready to be sent.
type payFX struct {
	*inv
	links   *stubLinks
	invoice int64
	approve int64
}

func newPay(t *testing.T) *payFX {
	t.Helper()
	i := newInv(t)
	links := &stubLinks{}
	i.e.s.WithPlatform(Platform{ProviderKinds: i.e.s.platform.ProviderKinds, Payments: links})
	i.allow(i.stripe, i.shared)
	req := i.mustRequest(i.alice, i.shared, i.app)
	expect(t, i.approve(i.owner, req.ID, i.stripe), http.StatusOK)
	expect(t, i.tryPublish(i.alice, i.shared, i.app), http.StatusOK)
	p := &payFX{inv: i, links: links, approve: req.ID}
	a := action(i.fresh(i.alice, i.shared, i.app), "add", map[string]any{"number": "A-1", "customer": "Acme", "total": 120.1, "status": "draft"})
	expect(t, i.alice.do(http.MethodPost, appPath(i.shared, i.app.ID, "/actions"), a), http.StatusOK)
	if err := i.e.db.QueryRow(`SELECT max(id) FROM app_rows WHERE app_id = ?`, i.app.ID).Scan(&p.invoice); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *payFX) send(u *user, operation string) {
	p.e.t.Helper()
	expect(p.e.t, p.trySend(u, operation), http.StatusOK)
}

func (p *payFX) trySend(u *user, operation string) *httptest.ResponseRecorder {
	p.e.t.Helper()
	a := action(p.fresh(p.alice, p.shared, p.app), "send", map[string]any{"id": p.invoice, "status": "sent"})
	a.Operation = operation
	return u.do(http.MethodPost, appPath(p.shared, p.app.ID, "/actions"), a)
}

func (p *payFX) work() { p.e.s.drain(context.Background()) }

func (p *payFX) row() (data string, revision int64, system string) {
	p.e.t.Helper()
	var sys *string
	if err := p.e.db.QueryRow(`SELECT data, revision, updated_system FROM app_rows WHERE id = ?`, p.invoice).Scan(&data, &revision, &sys); err != nil {
		p.e.t.Fatal(err)
	}
	if sys != nil {
		system = *sys
	}
	return data, revision, system
}

func (p *payFX) entries(u *user) EffectList {
	p.e.t.Helper()
	rec := u.do(http.MethodGet, appPath(p.shared, p.app.ID, "/effects"), nil)
	expect(p.e.t, rec, http.StatusOK)
	return decode[EffectList](p.e.t, rec)
}

func TestSendingAnInvoiceQueuesAPaymentLinkEntryAsTheUser(t *testing.T) {
	p := newPay(t)
	p.send(p.alice, "op-send-0001")
	var kind, payload string
	var user int64
	if err := p.e.db.QueryRow(`SELECT kind, payload, user_id FROM app_effects`).Scan(&kind, &payload, &user); err != nil {
		t.Fatal(err)
	}
	if kind != EffectCreatePaymentLink || user != p.alice.id {
		t.Fatalf("entry = %s as %d", kind, user)
	}
	// Filled in from the record: the exact amount as text, the currency, the text and where it goes.
	for _, want := range []string{`"slot":"payments"`, `"amount":"120.1"`, `"currency":"EUR"`, `"description":"Invoice A-1 for Acme"`, `"url_field":"pay_url"`, `"id_field":"payment_id"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload %s lacks %s", payload, want)
		}
	}
	if p.links.count() != 0 {
		t.Fatal("the request itself called the provider's side")
	}
}

func TestThePaymentLinkIsWrittenBackBySystemOnce(t *testing.T) {
	p := newPay(t)
	p.send(p.alice, "op-send-0001")
	p.work()
	if p.links.count() != 1 {
		t.Fatalf("calls = %d", p.links.count())
	}
	call := p.links.calls[0]
	if call.IntegrationID != p.stripe || call.AppID != p.app.ID || call.RecordID != p.invoice || call.SpaceID != p.shared.ID ||
		call.WorkspaceID != p.ws || call.Amount != "120.1" || call.Currency != "EUR" || call.Slot != "payments" || call.Description != "Invoice A-1 for Acme" {
		t.Fatalf("request = %+v", call)
	}
	data, rev, system := p.row()
	if !strings.Contains(data, `"pay_url":"https://pay.example.test/c/1"`) || !strings.Contains(data, `"payment_id":"cs_test_1"`) {
		t.Fatalf("record = %s", data)
	}
	if system != fmt.Sprintf("integration:%d", p.stripe) {
		t.Fatalf("updated_system = %q", system)
	}
	var actor *int64
	var actorSystem, action string
	if err := p.e.db.QueryRow(`SELECT actor, actor_system, action FROM app_row_history WHERE row_id = ? AND revision = ?`, p.invoice, rev).Scan(&actor, &actorSystem, &action); err != nil {
		t.Fatal(err)
	}
	if actor != nil || actorSystem != system || action != "payment_link.created" {
		t.Fatalf("history = %v %q %q", actor, actorSystem, action)
	}
	l := p.entries(p.alice)
	if len(l.Effects) != 1 || l.Effects[0].Status != StateDone || l.Effects[0].Detail.Amount != "120.10" || l.Effects[0].Detail.Currency != "EUR" {
		t.Fatalf("entries = %+v", l)
	}
	if l.Effects[0].Result["amount"] != "120.10" || l.Effects[0].Result["currency"] != "EUR" {
		t.Fatalf("result = %+v", l.Effects[0].Result)
	}
	// Nothing runs the entry again.
	p.work()
	if p.links.count() != 1 {
		t.Fatalf("calls after another round = %d", p.links.count())
	}
}

// A server that dies between the link and the note that the entry is done does the entry again:
// the integration's side finds what it made (here, the stub does as the real one does), and writing
// the link again is no change: no new revision.
func TestACrashAfterTheLinkIsMadeMakesNoSecondLinkAndNoSecondRevision(t *testing.T) {
	p := newPay(t)
	p.send(p.alice, "op-send-0001")
	crashed := false
	p.e.s.hooks.afterWork = func(e effectRow) error {
		if e.kind == EffectCreatePaymentLink && !crashed {
			crashed = true
			return errCrash
		}
		return nil
	}
	p.work()
	_, rev, _ := p.row()
	if st := p.entries(p.alice).Effects[0].Status; st != StateRunning && st != StatePending {
		t.Fatalf("the entry after the crash is %s", st)
	}
	// The worker's lease runs out, and another does the entry.
	p.e.s.clock = func() time.Time { return time.Now().Add(leaseTime + time.Second) }
	p.e.s.hooks.afterWork = nil
	p.work()
	if len(p.links.made) != 1 {
		t.Fatalf("%d links were made", len(p.links.made))
	}
	if p.links.count() != 2 {
		t.Fatalf("calls = %d, want the crashed one and the retry", p.links.count())
	}
	if _, rev2, _ := p.row(); rev2 != rev {
		t.Fatalf("revision %d -> %d: the link was written twice", rev, rev2)
	}
	if st := p.entries(p.alice).Effects[0].Status; st != StateDone {
		t.Fatalf("entry = %s", st)
	}
}

func TestAnApprovalRevokedBeforeTheEntryRunsStopsIt(t *testing.T) {
	for name, undo := range map[string]func(*payFX){
		"revoked":          func(p *payFX) { expect(p.e.t, p.decide(p.owner, p.ws, p.approve, "revoke", nil), http.StatusOK) },
		"switched off":     func(p *payFX) { p.exec(`UPDATE integrations SET disabled_at = CURRENT_TIMESTAMP`) },
		"space taken away": func(p *payFX) { p.exec(`DELETE FROM integration_spaces`) },
	} {
		t.Run(name, func(t *testing.T) {
			p := newPay(t)
			p.send(p.alice, "op-send-0001")
			undo(p)
			p.work()
			if p.links.count() != 0 {
				t.Fatal("the provider's side was asked")
			}
			l := p.entries(p.alice)
			if l.Effects[0].Status != StateFailed || l.Effects[0].Error == nil || l.Effects[0].Error.Code != apierr.CodeIntegrationUnavailable {
				t.Fatalf("entry = %+v", l.Effects[0])
			}
			if data, _, _ := p.row(); strings.Contains(data, "pay_url\":\"h") {
				t.Fatalf("the record got a link: %s", data)
			}
		})
	}
}

func (p *payFX) exec(q string, args ...any) {
	p.e.t.Helper()
	if _, err := p.e.db.Exec(q, args...); err != nil {
		p.e.t.Fatal(err)
	}
}

func TestAReleaseThatNoLongerAsksForTheLinkRefusesAnEntryOfAnOlderOne(t *testing.T) {
	p := newPay(t)
	p.send(p.alice, "op-send-0001")
	// A new release whose send action asks in another currency, approved: the entry the older one
	// queued has no authority under it.
	d, err := changed(t, `"amount_field": "total", "currency": "EUR"`, `"amount_field": "total", "currency": "USD"`)
	if err != nil {
		t.Fatal(err)
	}
	app := p.fresh(p.alice, p.shared, p.app)
	expect(t, p.alice.do(http.MethodPatch, appPath(p.shared, app.ID, ""), map[string]any{"version": app.Version, "definition": d}), http.StatusOK)
	expect(t, p.approve2(), http.StatusOK)
	expect(t, p.tryPublish(p.alice, p.shared, p.fresh(p.alice, p.shared, p.app)), http.StatusOK)
	p.work()
	if p.links.count() != 0 {
		t.Fatal("an entry of a release that asked for something else called the provider's side")
	}
	if e := p.entries(p.alice).Effects[0]; e.Status != StateFailed || e.Error.Code != apierr.CodeIntegrationUnavailable {
		t.Fatalf("entry = %+v", e)
	}
}

func (p *payFX) approve2() *httptest.ResponseRecorder {
	p.e.t.Helper()
	return p.approve3(p.mustRequest(p.alice, p.shared, p.app).ID)
}

func (p *payFX) approve3(id int64) *httptest.ResponseRecorder {
	return p.inv.approve(p.owner, id, p.stripe)
}

func TestFailuresOfTheLinkAreTheEntrysCodes(t *testing.T) {
	cases := map[string]struct {
		err       error
		code      apierr.Code
		transient bool
	}{
		"refused amount":  {&PaymentLinkError{Code: apierr.CodeIntegrationAmountRefused}, apierr.CodeIntegrationAmountRefused, false},
		"above the max":   {&PaymentLinkError{Code: apierr.CodeIntegrationAmountAboveMax, Params: apierr.Params{"max": "10.00", "currency": "EUR"}}, apierr.CodeIntegrationAmountAboveMax, false},
		"provider down":   {&PaymentLinkError{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}, apierr.CodeIntegrationProviderUnreachable, true},
		"another failure": {fmt.Errorf("the database was busy"), apierr.CodeAppEffectGaveUp, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := newPay(t)
			p.links.fail = tc.err
			p.send(p.alice, "op-send-0001")
			p.work()
			e := p.entries(p.alice).Effects[0]
			if tc.transient {
				// Tried again later, with the code of the last failure kept.
				if e.Status != StatePending || e.Attempts != 1 || e.Error == nil || e.Error.Code != tc.code {
					t.Fatalf("entry = %+v", e)
				}
				return
			}
			if e.Status != StateFailed || e.Error == nil || e.Error.Code != tc.code || !e.CanRetry {
				t.Fatalf("entry = %+v", e)
			}
			if tc.code == apierr.CodeIntegrationAmountAboveMax && e.Error.Params["currency"] != "EUR" {
				t.Fatalf("params = %v", e.Error.Params)
			}
			// The person who acted retries it once the problem is gone, and it makes the link.
			p.links.fail = nil
			expect(t, p.alice.do(http.MethodPost, appPath(p.shared, p.app.ID, fmt.Sprintf("/effects/%d/retry", e.ID)), map[string]any{}), http.StatusOK)
			p.work()
			if e := p.entries(p.alice).Effects[0]; e.Status != StateDone {
				t.Fatalf("after the retry: %+v", e)
			}
		})
	}
}

func TestAPaymentLinkNeedsAServerThatMakesThem(t *testing.T) {
	p := newPay(t)
	p.e.s.WithPlatform(Platform{})
	p.send(p.alice, "op-send-0001")
	p.work()
	if e := p.entries(p.alice).Effects[0]; e.Status == StateDone {
		t.Fatalf("entry = %+v", e)
	}
}

// previewSend previews the send action of the draft on one invoice, as the builder's preview does:
// on the rows it is given, which it keeps in memory.
func (p *payFX) previewSend(u *user, total any) *httptest.ResponseRecorder {
	p.e.t.Helper()
	body := struct {
		ClientAction
		Rows []PreviewRow `json:"rows"`
	}{
		ClientAction: action(p.app, "send", map[string]any{"id": 1, "status": "sent"}),
		Rows:         []PreviewRow{{ID: 1, Revision: 1, Data: map[string]any{"number": "A-9", "customer": "Zed", "total": total, "status": "draft"}}},
	}
	return u.do(http.MethodPost, appPath(p.shared, p.app.ID, "/actions/preview"), body)
}

// A preview says what the entry would ask, and never calls anyone.
func TestThePreviewSimulatesThePaymentLinkAndNeverCallsTheProvider(t *testing.T) {
	p := newPay(t)
	rec := p.previewSend(p.alice, 120.1)
	expect(t, rec, http.StatusOK)
	pr := decode[Preview](t, rec)
	if len(pr.Effects) != 1 {
		t.Fatalf("preview = %+v", pr)
	}
	v := pr.Effects[0]
	if v.Kind != EffectCreatePaymentLink || v.Status != StateSimulated || v.Error != nil || v.Detail.Currency != "EUR" ||
		v.Detail.Amount != "120.10" || v.Detail.Title != "Invoice A-9 for Zed" {
		t.Fatalf("effect = %+v", v)
	}
	p.work()
	if p.links.count() != 0 {
		t.Fatal("a preview called the provider's side")
	}
	if n := p.count(`SELECT count(*) FROM app_effects`); n != 0 {
		t.Fatalf("a preview queued %d entries", n)
	}
	if n := p.count(`SELECT count(*) FROM integration_refs`); n != 0 {
		t.Fatalf("a preview made %d refs", n)
	}
	// Those who may not design or publish the app have no preview.
	expect(t, p.previewSend(p.bob, 120.1), http.StatusForbidden)
	expect(t, p.previewSend(p.mallory, 120.1), http.StatusNotFound)
}

// floatNoise is 0.1 + 0.2, which a float holds as 0.30000000000000004 (variables: constants add
// exactly).
func floatNoise() float64 {
	a, b := 0.1, 0.2
	return a + b
}

func TestAPreviewRefusesAnAmountTheCurrencyCannotHold(t *testing.T) {
	p := newPay(t)
	for name, tc := range map[string]struct {
		total any
		code  apierr.Code
	}{
		"three decimals": {120.123, apierr.CodeIntegrationAmountRefused},
		"zero":           {0, apierr.CodeIntegrationAmountRefused},
		"negative":       {-5, apierr.CodeIntegrationAmountRefused},
		"float noise":    {floatNoise(), apierr.CodeIntegrationAmountRefused},
	} {
		t.Run(name, func(t *testing.T) {
			rec := p.previewSend(p.alice, tc.total)
			expect(t, rec, http.StatusOK)
			pr := decode[Preview](t, rec)
			if len(pr.Effects) != 1 || pr.Effects[0].Error == nil || pr.Effects[0].Error.Code != tc.code {
				t.Fatalf("preview = %+v", pr)
			}
		})
	}
}
