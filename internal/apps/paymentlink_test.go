package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/paylink"
)

// stubLinks stands in for the integrations' side of a payment link (package integrations tests the
// real one against a fake Stripe): it makes a link once per entry, as that side does, and counts
// what it was asked.
type stubLinks struct {
	mu    sync.Mutex
	calls []paylink.Request
	made  map[int64]paylink.Link
	fail  error
	// onOpen, if set, runs while the integration's side is asked (the entry is mid-call).
	onOpen func(req paylink.Request)
}

func (s *stubLinks) OpenPaymentLink(_ context.Context, req paylink.Request) (paylink.Link, error) {
	s.mu.Lock()
	hook := s.onOpen
	s.mu.Unlock()
	if hook != nil {
		hook(req)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if s.fail != nil {
		return paylink.Link{}, s.fail
	}
	if l, ok := s.made[req.EffectID]; ok {
		return l, nil
	}
	if s.made == nil {
		s.made = map[int64]paylink.Link{}
	}
	n := len(s.made) + 1
	l := paylink.Link{URL: fmt.Sprintf("https://pay.example.test/c/%d", n), ObjectID: fmt.Sprintf("cs_test_%d", n), AmountMinor: 12010, Currency: "EUR"}
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
		"refused amount":  {&paylink.Error{Code: apierr.CodeIntegrationAmountRefused}, apierr.CodeIntegrationAmountRefused, false},
		"above the max":   {&paylink.Error{Code: apierr.CodeIntegrationAmountAboveMax, Params: apierr.Params{"max": "10.00", "currency": "EUR"}}, apierr.CodeIntegrationAmountAboveMax, false},
		"provider down":   {&paylink.Error{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}, apierr.CodeIntegrationProviderUnreachable, true},
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

// sendTotal sends the invoice again with another total: another entry for the same record.
func (p *payFX) sendTotal(operation string, total float64) {
	p.e.t.Helper()
	a := action(p.fresh(p.alice, p.shared, p.app), "send", map[string]any{"id": p.invoice, "status": "sent", "total": total})
	a.Operation = operation
	expect(p.e.t, p.alice.do(http.MethodPost, appPath(p.shared, p.app.ID, "/actions"), a), http.StatusOK)
}

func (p *payFX) effectIDs() (ids []int64) {
	p.e.t.Helper()
	rows, err := p.e.db.Query(`SELECT id FROM app_effects ORDER BY id`)
	if err != nil {
		p.e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			p.e.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// An entry that is tried again after a newer one for the same record has written its link must not
// put its own, for an earlier amount, over it.
func TestAnOlderEntryTriedAgainNeverReplacesTheNewerLink(t *testing.T) {
	p := newPay(t)
	p.links.fail = &paylink.Error{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}
	p.send(p.alice, "op-send-0001")
	p.work() // the first entry fails for now and waits to be tried again
	p.links.fail = nil
	p.sendTotal("op-send-0002", 50)
	p.work() // the newer entry is the only one due: it makes its link
	ids := p.effectIDs()
	if len(ids) != 2 {
		t.Fatalf("entries = %v", ids)
	}
	data, rev, _ := p.row()
	if !strings.Contains(data, `"pay_url":"https://pay.example.test/c/1"`) {
		t.Fatalf("record = %s", data)
	}
	calls := p.links.count()

	// The older one is due again: it has been replaced, and neither asks nor writes.
	p.e.s.clock = func() time.Time { return time.Now().Add(time.Hour) }
	p.work()
	if p.links.count() != calls {
		t.Fatalf("the older entry asked for a link again: %d calls, was %d", p.links.count(), calls)
	}
	if after, rev2, _ := p.row(); after != data || rev2 != rev {
		t.Fatalf("the older entry changed the record: %s (revision %d), was %s (revision %d)", after, rev2, data, rev)
	}
	for _, e := range p.entries(p.alice).Effects {
		if e.Status != StateDone {
			t.Fatalf("entry %d is %s", e.ID, e.Status)
		}
		if e.ID == ids[0] && e.Result["superseded"] != true {
			t.Fatalf("the older entry's result = %+v", e.Result)
		}
	}
}

// The check is made again where the link is written, under the write lock: a newer entry that is
// queued while the older one is asking the provider still wins.
func TestANewerEntryQueuedWhileTheOlderAsksTheProviderWins(t *testing.T) {
	p := newPay(t)
	p.send(p.alice, "op-send-0001")
	first := p.effectIDs()[0]
	queued := false
	p.links.onOpen = func(req paylink.Request) {
		if req.EffectID == first && !queued {
			queued = true
			p.sendTotal("op-send-0002", 50)
		}
	}
	p.work()
	data, _, _ := p.row()
	if strings.Contains(data, `"pay_url":"https://pay.example.test/c/1"`) {
		t.Fatalf("the older entry wrote its link over the newer entry's turn: %s", data)
	}
	if !strings.Contains(data, `"pay_url":"https://pay.example.test/c/2"`) {
		t.Fatalf("record = %s", data)
	}
	// The older entry never wrote, even for a moment: the record's history has the newer link only.
	if n := p.count(`SELECT count(*) FROM app_row_history WHERE row_id = ? AND action = 'payment_link.created'`, p.invoice); n != 1 {
		t.Fatalf("%d link writes in the record's history, want 1", n)
	}
}

// What an entry's description sends out was approved with the release that queued it: a live
// contract that no longer names a field (or the id) the entry filled in stops it.
func TestAnEntryWhoseDescriptionSendsWhatTheLiveContractNoLongerApprovesIsRefused(t *testing.T) {
	p := newPay(t)
	p.send(p.alice, "op-send-0001")
	if !strings.Contains(p.payloadOf(), `"describes":["customer","number"]`) {
		t.Fatalf("payload = %s", p.payloadOf())
	}
	d, err := changed(t, `"description": "Invoice {{number}} for {{customer}}"`, `"description": "Invoice {{number}}"`)
	if err != nil {
		t.Fatal(err)
	}
	app := p.fresh(p.alice, p.shared, p.app)
	expect(t, p.alice.do(http.MethodPatch, appPath(p.shared, app.ID, ""), map[string]any{"version": app.Version, "definition": d}), http.StatusOK)
	expect(t, p.approve2(), http.StatusOK)
	expect(t, p.tryPublish(p.alice, p.shared, p.fresh(p.alice, p.shared, p.app)), http.StatusOK)
	p.work()
	if p.links.count() != 0 {
		t.Fatal("an entry that sends a field the live contract doesn't name called the provider's side")
	}
	if e := p.entries(p.alice).Effects[0]; e.Status != StateFailed || e.Error.Code != apierr.CodeIntegrationUnavailable {
		t.Fatalf("entry = %+v", e)
	}
}

// Wording alone is not in the contract: the same fields in other words still run.
func TestAnEntryWhoseDescriptionOnlyChangedWordingStillRuns(t *testing.T) {
	p := newPay(t)
	p.send(p.alice, "op-send-0001")
	d, err := changed(t, `"description": "Invoice {{number}} for {{customer}}"`, `"description": "Bill {{customer}}, invoice {{number}}"`)
	if err != nil {
		t.Fatal(err)
	}
	app := p.fresh(p.alice, p.shared, p.app)
	expect(t, p.alice.do(http.MethodPatch, appPath(p.shared, app.ID, ""), map[string]any{"version": app.Version, "definition": d}), http.StatusOK)
	// The contract holds no wording: the approval still covers the new release.
	expect(t, p.tryPublish(p.alice, p.shared, p.fresh(p.alice, p.shared, p.app)), http.StatusOK)
	p.work()
	if p.links.count() != 1 {
		t.Fatalf("calls = %d", p.links.count())
	}
	if e := p.entries(p.alice).Effects[0]; e.Status != StateDone {
		t.Fatalf("entry = %+v", e)
	}
}

func (p *payFX) payloadOf() (payload string) {
	p.e.t.Helper()
	if err := p.e.db.QueryRow(`SELECT payload FROM app_effects ORDER BY id LIMIT 1`).Scan(&payload); err != nil {
		p.e.t.Fatal(err)
	}
	return payload
}

// ----- the contract is checked whole, as of now -----

// release publishes the definition, edited from the invoices' text by replacing each from with its to
// (pairs of two), as the next release, approved.
func (p *payFX) release(pairs ...string) { p.e.t.Helper(); p.releaseAck(nil, pairs...) }

// releaseAck is release with the schema changes the publisher acknowledges.
func (p *payFX) releaseAck(ack []SchemaChange, pairs ...string) {
	p.e.t.Helper()
	text := invoiceJSON
	for i := 0; i+1 < len(pairs); i += 2 {
		if !strings.Contains(text, pairs[i]) {
			p.e.t.Fatalf("the invoice definition has no %q", pairs[i])
		}
		text = strings.Replace(text, pairs[i], pairs[i+1], 1)
	}
	var d Definition
	if err := json.Unmarshal([]byte(text), &d); err != nil {
		p.e.t.Fatal(err)
	}
	if _, err := d.Check(); err != nil {
		p.e.t.Fatal(err)
	}
	app := p.fresh(p.alice, p.shared, p.app)
	expect(p.e.t, p.alice.do(http.MethodPatch, appPath(p.shared, app.ID, ""), map[string]any{"version": app.Version, "definition": d}), http.StatusOK)
	expect(p.e.t, p.approve2(), http.StatusOK)
	expect(p.e.t, p.tryPublish(p.alice, p.shared, p.fresh(p.alice, p.shared, p.app), ack...), http.StatusOK)
}

// refused is the one entry's failure with the integration unavailable, and no call to the provider.
func (p *payFX) refused() { p.e.t.Helper(); p.refusedWith(apierr.CodeIntegrationUnavailable) }

// refusedWith is the one entry's failure with the code, and no call to the provider.
func (p *payFX) refusedWith(want apierr.Code) {
	p.e.t.Helper()
	p.work()
	if p.links.count() != 0 {
		p.e.t.Fatal("the entry called the provider's side")
	}
	if e := p.entries(p.alice).Effects[0]; e.Status != StateFailed || e.Error == nil || e.Error.Code != want {
		p.e.t.Fatalf("entry = %+v, want %s", e, want)
	}
	if data, _, _ := p.row(); strings.Contains(data, "pay_url\":\"h") {
		p.e.t.Fatalf("the record got a link: %s", data)
	}
}

// What an admin approved is the whole link: the kind of action that asks, the rule the record must
// meet, where the currency is read from and what the amount may be. An entry that an older release
// queued has no authority under a release that approved something else of these.
func TestAnEntryQueuedUnderAnotherContractOfTheSameLinkIsRefused(t *testing.T) {
	const effect = `"amount_field": "total", "currency": "EUR",`
	for name, tc := range map[string]struct{ from, to string }{
		// The action that asked was an update; the release's is an insert (who may trigger it differs).
		"another kind of action": {`"send": {"kind": "update"`, `"send": {"kind": "insert"`},
		// A rule the record must meet was added and the record doesn't meet it.
		"a rule the record fails": {effect, `"amount_field": "total", "currency": "EUR", "when": {"field": "status", "op": "eq", "value": "overdue"},`},
		// The currency now comes from the record's choice, which the entry never read.
		"the currency read from a field": {effect, `"amount_field": "total", "currency_field": "currency",`},
		// The largest amount the field takes was lowered below the entry's 120.10.
		"a lowered maximum": {`{"name": "total", "type": "number", "required": true}`, `{"name": "total", "type": "number", "required": true, "max": 100}`},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPay(t)
			p.send(p.alice, "op-send-0001")
			var ack []SchemaChange
			if strings.Contains(tc.to, `"max": 100`) { // a lowered maximum narrows the field: the publisher says they know
				ack = []SchemaChange{{Change: "field_narrowed", Field: "total"}}
			}
			p.releaseAck(ack, tc.from, tc.to)
			if name == "a rule the record fails" {
				// The rule is the contract's; the record no longer meets it.
				p.refusedWith(apierr.CodeIntegrationConditionChanged)
			} else {
				p.refused()
			}
		})
	}
}

// The same changes that the record satisfies are no obstacle: the entry runs.
func TestAnEntryUnderAContractItSatisfiesStillRuns(t *testing.T) {
	const effect = `"amount_field": "total", "currency": "EUR",`
	for name, tc := range map[string]struct{ from, to string }{
		"a rule the record meets": {effect, `"amount_field": "total", "currency": "EUR", "when": {"field": "status", "op": "eq", "value": "sent"},`},
		"a maximum above it":      {`{"name": "total", "type": "number", "required": true}`, `{"name": "total", "type": "number", "required": true, "max": 1000}`},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPay(t)
			p.send(p.alice, "op-send-0001")
			p.release(tc.from, tc.to)
			p.work()
			if e := p.entries(p.alice).Effects[0]; e.Status != StateDone || p.links.count() != 1 {
				t.Fatalf("entry = %+v, %d calls", e, p.links.count())
			}
		})
	}
}

// ----- the write is checked where it is made -----

// The provider takes time, and what authorized the entry may change in it: nothing is written to the
// record unless everything still holds under the write's own lock.
func TestWhatChangesWhileTheProviderIsAskedStopsTheWrite(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*payFX)
		code   apierr.Code
	}{
		"approval revoked":         {func(p *payFX) { expect(p.e.t, p.decide(p.owner, p.ws, p.approve, "revoke", nil), http.StatusOK) }, apierr.CodeIntegrationUnavailable},
		"integration switched off": {func(p *payFX) { p.exec(`UPDATE integrations SET disabled_at = CURRENT_TIMESTAMP`) }, apierr.CodeIntegrationUnavailable},
		"space taken away":         {func(p *payFX) { p.exec(`DELETE FROM integration_spaces`) }, apierr.CodeIntegrationUnavailable},
		"release changed": {func(p *payFX) {
			p.release(`"amount_field": "total", "currency": "EUR"`, `"amount_field": "total", "currency": "USD"`)
		}, apierr.CodeIntegrationUnavailable},
		"app moved": {func(p *payFX) {
			p.exec(`UPDATE apps SET space_id = ? WHERE id = ?`, p.old.ID, p.app.ID)
		}, apierr.CodeAppEffectAppAccess},
		"the user's access ended": {func(p *payFX) {
			p.exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, p.shared.ID, p.alice.id)
		}, apierr.CodeAppEffectAppAccess},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPay(t)
			p.send(p.alice, "op-send-0001")
			_, rev, _ := p.row()
			done := false
			p.links.onOpen = func(paylink.Request) {
				if !done {
					done = true
					tc.change(p)
				}
			}
			p.work()
			if p.links.count() != 1 {
				t.Fatalf("calls = %d, want the one that was in flight", p.links.count())
			}
			if data, rev2, system := p.row(); strings.Contains(data, "pay_url\":\"h") || rev2 != rev || system != "" {
				t.Fatalf("the record was written: %s, revision %d (was %d), by %q", data, rev2, rev, system)
			}
			if n := p.count(`SELECT count(*) FROM app_row_history WHERE row_id = ? AND action = 'payment_link.created'`, p.invoice); n != 0 {
				t.Fatalf("%d link writes in the history", n)
			}
			var state, errCode string
			if err := p.e.db.QueryRow(`SELECT state, coalesce(error_code, '') FROM app_effects`).Scan(&state, &errCode); err != nil {
				t.Fatal(err)
			}
			if state != StateFailed || errCode != string(tc.code) {
				t.Fatalf("entry = %s, %s; want failed, %s", state, errCode, tc.code)
			}
		})
	}
}

// ----- what an entry shows once its fields are gone -----

// A payment link's amount and currency came from the record's fields: once the live release no
// longer holds them as they were, neither the result nor an error's params show them.
func TestAnEntrysAmountAndCurrencyAreWithheldOnceTheirFieldsAreGone(t *testing.T) {
	s := &Service{}
	all := func(int64) bool { return true }
	caller := Actor{UserID: 1}
	holds := Collection{Fields: []Field{{Name: "total", Type: TypeNumber}, {Name: "currency", Type: TypeChoice, Options: []string{"EUR"}}}}
	retyped := Collection{Fields: []Field{{Name: "total", Type: TypeString}, {Name: "currency", Type: TypeChoice, Options: []string{"EUR"}}}}
	gone := Collection{}
	pl := payload{SpaceID: 7, Slot: "payments", Amount: "120.1", Currency: "EUR", Uses: map[string]string{"total": TypeNumber, "currency": TypeChoice}}
	done := effectRow{state: StateDone, kind: EffectCreatePaymentLink, payload: pl,
		result: sql.NullString{Valid: true, String: `{"workspace_id":1,"space_id":7,"amount":"120.10","currency":"EUR"}`}}
	failed := effectRow{state: StateFailed, kind: EffectCreatePaymentLink, payload: pl, errCode: sql.NullString{Valid: true, String: string(apierr.CodeIntegrationAmountAboveMax)},
		errParams: sql.NullString{Valid: true, String: `{"max":"100.00","currency":"EUR"}`}}
	for name, tc := range map[string]struct {
		live *Collection
		show bool
	}{"holds": {&holds, true}, "retyped": {&retyped, false}, "gone": {&gone, false}, "no live release": {nil, false}} {
		t.Run(name, func(t *testing.T) {
			v, err := s.view(done, caller, all, tc.live)
			if err != nil {
				t.Fatal(err)
			}
			_, hasAmount := v.Result["amount"]
			_, hasCurrency := v.Result["currency"]
			if hasAmount != tc.show || hasCurrency != tc.show || v.Detail.Amount != map[bool]string{true: "120.10", false: ""}[tc.show] {
				t.Fatalf("result = %v, detail = %+v", v.Result, v.Detail)
			}
			v, err = s.view(failed, caller, all, tc.live)
			if err != nil {
				t.Fatal(err)
			}
			_, hasMax := v.Error.Params["max"]
			_, hasCode := v.Error.Params["currency"]
			if hasMax != tc.show || hasCode != tc.show {
				t.Fatalf("params = %v", v.Error.Params)
			}
		})
	}
}

// ----- a preview refuses what the run would -----

// A currency of whole units (ISK) takes no decimals: the preview says so, as the entry would fail.
func TestAPreviewRefusesDecimalsInACurrencyOfWholeUnits(t *testing.T) {
	p := newPay(t)
	d, err := changed(t, `"amount_field": "total", "currency": "EUR"`, `"amount_field": "total", "currency": "ISK"`)
	if err != nil {
		t.Fatal(err)
	}
	app := p.fresh(p.alice, p.shared, p.app)
	expect(t, p.alice.do(http.MethodPatch, appPath(p.shared, app.ID, ""), map[string]any{"version": app.Version, "definition": d}), http.StatusOK)
	app = p.fresh(p.alice, p.shared, p.app)
	for total, want := range map[any]apierr.Code{5.5: apierr.CodeIntegrationAmountRefused, 500: ""} {
		body := struct {
			ClientAction
			Rows []PreviewRow `json:"rows"`
		}{
			ClientAction: action(app, "send", map[string]any{"id": 1, "status": "sent"}),
			Rows:         []PreviewRow{{ID: 1, Revision: 1, Data: map[string]any{"number": "A-9", "customer": "Zed", "total": total, "status": "draft"}}},
		}
		rec := p.alice.do(http.MethodPost, appPath(p.shared, app.ID, "/actions/preview"), body)
		expect(t, rec, http.StatusOK)
		pr := decode[Preview](t, rec)
		var got apierr.Code
		if len(pr.Effects) == 1 && pr.Effects[0].Error != nil {
			got = pr.Effects[0].Error.Code
		}
		if len(pr.Effects) != 1 || got != want {
			t.Fatalf("total %v: preview = %+v, want %q", total, pr, want)
		}
	}
}

// ----- the newest entry wins, whatever has been cleaned up -----

// The newest entry's mark outlives the entries: an older one tried again after the newer entry was
// cleaned up still finds it was replaced.
func TestAnOlderEntryIsStillSupersededOnceTheNewerOneIsCleanedUp(t *testing.T) {
	p := newPay(t)
	p.links.fail = &paylink.Error{Code: apierr.CodeIntegrationProviderUnreachable, Transient: true}
	p.send(p.alice, "op-send-0001")
	p.work()
	p.links.fail = nil
	p.sendTotal("op-send-0002", 50)
	p.work()
	ids := p.effectIDs()
	data, rev, _ := p.row()
	if len(ids) != 2 || !strings.Contains(data, `"pay_url":"https://pay.example.test/c/1"`) {
		t.Fatalf("entries = %v, record = %s", ids, data)
	}
	p.exec(`DELETE FROM app_effects WHERE id = ?`, ids[1]) // the retention sweep removed it
	calls := p.links.count()
	p.e.s.clock = func() time.Time { return time.Now().Add(time.Hour) }
	p.work()
	if p.links.count() != calls {
		t.Fatalf("the older entry asked for a link again: %d calls, was %d", p.links.count(), calls)
	}
	if after, rev2, _ := p.row(); after != data || rev2 != rev {
		t.Fatalf("the older entry changed the record: %s (revision %d), was %s (revision %d)", after, rev2, data, rev)
	}
	if e := p.entries(p.alice).Effects[0]; e.Status != StateDone || e.Result["superseded"] != true {
		t.Fatalf("entry = %+v", e)
	}
}

// A newer entry that shares either of the two fields with an older one replaces it: the record's link
// is the newer one's, however the two entries name their other field.
func TestANewerEntryOfEitherSharedFieldSupersedesAnOlderOne(t *testing.T) {
	p := newPay(t)
	ctx := context.Background()
	older := effectRow{id: 10, appID: p.app.ID, recordID: p.invoice, payload: payload{URLField: "pay_url", IDField: "payment_id"}}
	for name, tc := range map[string]struct {
		url, id string
		newest  int64
		want    bool
	}{
		"both fields":           {"pay_url", "payment_id", 11, true},
		"the link field only":   {"pay_url", "payment_ref", 11, true},
		"the id field only":     {"pay_link", "payment_id", 11, true},
		"neither field":         {"pay_link", "payment_ref", 11, false},
		"an older entry's mark": {"pay_url", "payment_id", 9, false},
	} {
		t.Run(name, func(t *testing.T) {
			p.exec(`DELETE FROM app_payment_link_latest`)
			tx, err := p.e.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := notePaymentLinkEntry(ctx, tx, p.app.ID, p.invoice, tc.newest, payload{URLField: tc.url, IDField: tc.id}); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			got, err := p.e.s.paymentLinkSuperseded(ctx, p.e.db, older)
			if err != nil || got != tc.want {
				t.Fatalf("superseded = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	// The mark only moves forward: a late write of an older entry's number doesn't take it back.
	tx, err := p.e.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{20, 15} {
		if err := notePaymentLinkEntry(ctx, tx, p.app.ID, p.invoice, id, payload{URLField: "pay_url", IDField: "payment_id"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := p.count(`SELECT count(*) FROM app_payment_link_latest WHERE effect_id = 20`); n != 2 {
		t.Fatalf("%d fields marked by the newest entry, want 2", n)
	}
}

// ----- review: the record's own change, one snapshot, and the app's place -----

// An ordinary edit of the record can make the rule of the action's payment link fail after the entry
// was queued. That is not an admin's matter: the entry fails with the code that says the record no
// longer meets the action's condition, and nothing is created.
func TestAnEditThatBreaksTheLinksConditionSaysSoAndCreatesNothing(t *testing.T) {
	p := newPay(t)
	p.release(`"amount_field": "total", "currency": "EUR",`, `"amount_field": "total", "currency": "EUR", "when": {"field": "status", "op": "eq", "value": "sent"},`)
	p.send(p.alice, "op-send-0001")
	p.exec(`UPDATE app_rows SET data = json_set(data, '$.status', 'draft') WHERE id = ?`, p.invoice)
	p.refusedWith(apierr.CodeIntegrationConditionChanged)
}

// Every read of an entry's authorization before the provider is called is of one snapshot. The user
// may not write the space when the entry is looked at; before its checks go on, the user is made an
// editor and the approval is revoked (two later states, which never held together with the first).
// Reading the access through the pool saw the user an editor while the binding, read in the
// transaction, was still the approved one: no state existed in which both held, and the provider
// was asked.
func TestThePreAuthorizationReadsOneSnapshot(t *testing.T) {
	p := newPay(t)
	p.send(p.alice, "op-send-0001")
	p.exec(`UPDATE space_members SET role = ? WHERE space_id = ? AND user_id = ?`, access.RoleViewer, p.shared.ID, p.alice.id)
	changed := false
	p.e.s.hooks.afterPaymentSnapshotRead = func() {
		if changed {
			return
		}
		changed = true
		p.exec(`UPDATE space_members SET role = ? WHERE space_id = ? AND user_id = ?`, access.RoleEditor, p.shared.ID, p.alice.id)
		expect(p.e.t, p.decide(p.owner, p.ws, p.approve, "revoke", nil), http.StatusOK)
	}
	p.work()
	if !changed {
		t.Fatal("the hook never ran")
	}
	if p.links.count() != 0 {
		t.Fatalf("the provider's side was asked %d times on a mix of two states", p.links.count())
	}
	if e := p.entries(p.alice).Effects[0]; e.Status != StateFailed || e.Error == nil || e.Error.Code != apierr.CodeAppEffectAppAccess {
		t.Fatalf("entry = %+v", e)
	}
}

// A move clears an app's approvals, so an entry queued before it must not run after an admin approves
// the app again in the new space: it asked for what was approved in the old one.
func TestAnEntryQueuedBeforeAMoveNeverRunsAfterIt(t *testing.T) {
	p := newPay(t)
	p.send(p.alice, "op-send-0001")

	next := p.e.createShared(p.owner, p.ws, "Second")
	p.e.addMember(p.owner, next, p.alice, access.RoleEditor)
	app := p.fresh(p.alice, p.shared, p.app)
	expect(t, p.alice.do(http.MethodPost, appPath(p.shared, app.ID, "/move"), map[string]any{"version": app.Version, "space_id": next.ID}), http.StatusOK)
	p.allow(p.stripe, next)
	req := p.mustRequest(p.alice, next, p.app)
	expect(t, p.inv.approve(p.owner, req.ID, p.stripe), http.StatusOK)

	p.work()
	if p.links.count() != 0 {
		t.Fatalf("the provider's side was asked %d times for an entry of the old space", p.links.count())
	}
	var state, code string
	if err := p.e.db.QueryRow(`SELECT state, coalesce(error_code, '') FROM app_effects`).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != StateFailed || code != string(apierr.CodeAppEffectSpaceChanged) {
		t.Fatalf("entry = %s, %s; want failed, %s", state, code, apierr.CodeAppEffectSpaceChanged)
	}
	if data, _, _ := p.row(); strings.Contains(data, "pay_url\":\"h") {
		t.Fatalf("the record got a link: %s", data)
	}
}
