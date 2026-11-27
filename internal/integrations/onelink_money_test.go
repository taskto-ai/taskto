package integrations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/integrations/testprovider"
	"taskto.ai/taskto/internal/paylink"
)

func paylinkRequest(p *pay, record, effect int64, slot string) paylink.Request {
	return paylink.Request{WorkspaceID: p.ws, IntegrationID: p.in.ID, SpaceID: p.space.ID, AppID: p.app.ID, RecordID: record, EffectID: effect,
		Slot: slot, URLField: "pay_url", AmountField: "total", CurrencyField: "currency", Amount: "10", Currency: "EUR", Description: "Invoice"}
}

func (p *pay) actRec(operation, name string, ctx map[string]any) *httptest.ResponseRecorder {
	p.t.Helper()
	a := apps.ClientAction{Name: name, SurfaceID: apps.SurfaceID(p.app.ID), SourceComponentID: "add", Context: ctx, Operation: operation}
	return p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/actions", p.app.ID)), a)
}

func (p *pay) expires(session string) {
	p.t.Helper()
	evs, err := p.fake.Expire(session)
	if err != nil {
		p.t.Fatal(err)
	}
	p.deliver(p.sender(p.hookID(), stripeHook), evs)
	p.drain()
}

func TestAnInvoicesMoneyCantChangeWhileItsLinkMayBePaid(t *testing.T) {
	p := newPayWith(t, publicURL, invoicingDefinition(t))
	record := p.templateInvoice(120)
	p.startWorker()
	p.act("op-inv-send-01", "send_invoice", map[string]any{"id": record, "status": "sent"})
	p.allEnded(1)
	first := p.field(record, "payment_id").(string)

	for name, change := range map[string]map[string]any{
		"the total":                     {"total": 130},
		"the currency":                  {"currency": "USD"},
		"the total, by the send action": {"total": 130, "status": "sent"},
	} {
		action := "edit_invoice"
		if change["status"] != nil {
			action = "send_invoice"
		}
		ctx := map[string]any{"id": record}
		for k, v := range change {
			ctx[k] = v
		}
		expectCodeNonFatal(t, p.actRec("", action, ctx), http.StatusUnprocessableEntity, string(apierr.CodeAppPaymentLinkOpen), name)
	}
	if p.field(record, "total") != float64(120) || p.field(record, "currency") != "EUR" {
		t.Fatalf("the invoice's money changed: %v %v", p.field(record, "total"), p.field(record, "currency"))
	}
	expect(t, p.actRec("", "edit_invoice", map[string]any{"id": record, "customer": "Acme Ltd"}), http.StatusOK)

	p.expires(first)
	expect(t, p.actRec("", "edit_invoice", map[string]any{"id": record, "total": 130}), http.StatusOK)
	p.act("op-inv-repl-01", "replace_link", map[string]any{"id": record, "pay_url": "", "payment_id": ""})
	if es := p.allEnded(2); es[1].state != apps.StateDone {
		t.Fatalf("the replacement: %+v", es[1])
	}
	refs := p.refs()
	if len(refs) != 2 || refs[0].state != "expired" || refs[1].state != "open" || refs[1].amount != 13000 || len(p.posts()) != 2 {
		t.Fatalf("refs = %+v, %d links made", refs, len(p.posts()))
	}
}

func TestAnInvoicesMoneyCantChangeWhileItsLinkIsBeingMade(t *testing.T) {
	p := newPayWith(t, publicURL, invoicingDefinition(t))
	record := p.templateInvoice(120)
	p.act("op-inv-send-01", "send_invoice", map[string]any{"id": record, "status": "sent"})
	expectCode(t, p.actRec("", "edit_invoice", map[string]any{"id": record, "total": 130}), http.StatusUnprocessableEntity, string(apierr.CodeAppPaymentLinkOpen))
	p.startWorker()
	if es := p.allEnded(1); es[0].state != apps.StateDone {
		t.Fatalf("the send: %+v", es[0])
	}
	if refs := p.refs(); len(refs) != 1 || refs[0].amount != 12000 || p.field(record, "total") != float64(120) {
		t.Fatalf("refs = %+v, total %v", refs, p.field(record, "total"))
	}
}

func TestARetriedEntryNeverAsksMoneyTheRecordNoLongerHas(t *testing.T) {
	p := newPayWith(t, publicURL, invoicingDefinition(t))
	record := p.templateInvoice(120)
	p.exec(`UPDATE integrations SET disabled_at = CURRENT_TIMESTAMP`)
	p.startWorker()
	p.act("op-inv-send-01", "send_invoice", map[string]any{"id": record, "status": "sent"})
	if es := p.allEnded(1); es[0].state != apps.StateFailed {
		t.Fatalf("the send to a switched-off integration: %+v", es[0])
	}
	expect(t, p.actRec("", "edit_invoice", map[string]any{"id": record, "total": 130}), http.StatusOK)
	p.exec(`UPDATE integrations SET disabled_at = NULL`)
	expect(t, p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/effects/%d/retry", p.app.ID, p.entries()[0].id)), map[string]any{}), http.StatusOK)
	p.waitFor("the retry ended", func() bool { es := p.entries(); return es[0].state == apps.StateFailed && es[0].attempts > 0 })
	if es := p.entries(); es[0].code != string(apierr.CodeIntegrationAmountChanged) || len(p.posts()) != 0 || len(p.refs()) != 0 {
		t.Fatalf("the retry: %+v, %d links made", es[0], len(p.posts()))
	}
}

func TestNoNewLinkBesideAPaidRefsOpenObject(t *testing.T) {
	for name, tc := range map[string]struct {
		move func(p *pay)
		code apierr.Code
	}{
		"another integration": {func(p *pay) {
			body := stripeBody("other")
			body["secrets"] = map[string]string{"api_key": p.fake.APIKey(), "webhook_secret": hookFor("other")}
			other := p.create(p.owner, p.ws, body)
			p.exec(`UPDATE integration_refs SET integration_id = ?`, other.ID)
		}, apierr.CodeIntegrationChanged},
		"another account": {func(p *pay) { p.exec(`UPDATE integration_refs SET account_id = 'acct_elsewhere'`) }, apierr.CodeIntegrationAccountChanged},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, publicURL)
			record, r := p.sentInvoice(10, "EUR", "op-paid-open-1")
			p.exec(`UPDATE integration_refs SET state = 'paid', paid_object_id = 'cs_test_abandoned', payment_id = 'pi_test_1' WHERE id = ?`, r.id)
			tc.move(p)
			p.act("op-paid-open-2", "send", map[string]any{"id": record, "status": "sent", "customer": "Other"})
			es := p.allEnded(2)
			if es[1].state != apps.StateFailed || es[1].code != string(tc.code) {
				t.Fatalf("the second entry: %+v", es[1])
			}
			if n := len(p.posts()); n != 1 || len(p.refs()) != 1 {
				t.Fatalf("%d links made beside an open one; refs %+v", n, p.refs())
			}
		})
	}
}

func TestALinkGivenAgainPaysIntoTheLiveSlot(t *testing.T) {
	p := newPay(t, publicURL)
	record, r := p.sentInvoice(10, "EUR", "op-slot-0001")
	p.exec(`UPDATE integration_refs SET slot = 'old_payments' WHERE id = ?`, r.id)
	p.act("op-slot-0002", "send", map[string]any{"id": record, "status": "sent", "customer": "Other"})
	if es := p.allEnded(2); es[1].state != apps.StateDone || len(p.posts()) != 1 {
		t.Fatalf("the second entry: %+v, %d links", es[1], len(p.posts()))
	}
	var slot string
	if err := p.db.QueryRow(`SELECT slot FROM integration_refs WHERE id = ?`, r.id).Scan(&slot); err != nil || slot != "payments" {
		t.Fatalf("the ref's slot is %q, %v", slot, err)
	}
	p.pays(r.object)
	if got := p.field(record, "status"); got != "paid" {
		t.Fatalf("the payment wasn't written: status %v; events %+v", got, p.eventRows())
	}
}

func TestAnAllExpiredRefIsReplacedForNewMoney(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-expired-0001")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	p.fake.ClearFaults()
	sessions := p.fake.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("%d sessions", len(sessions))
	}
	if _, err := p.fake.Expire(sessions[0].ID); err != nil {
		t.Fatal(err)
	}
	p.exec(`UPDATE integration_ref_attempts SET outcome = 'expired'`)
	p.exec(`UPDATE app_effects SET state = 'failed', error_code = ?`, string(apierr.CodeIntegrationOutcomeUnknown))
	old := p.refs()[0]

	p.later(time.Minute)
	p.startWorker()
	p.act("op-expired-0002", "send", map[string]any{"id": record, "status": "sent", "total": 12})
	p.waitFor("the newer entry", func() bool {
		es := p.entries()
		return len(es) == 2 && (es[1].state == apps.StateDone || es[1].state == apps.StateFailed)
	})
	if es := p.entries(); es[1].state != apps.StateDone {
		t.Fatalf("the newer entry: %+v", es[1])
	}
	refs := p.refs()
	if len(refs) != 2 || refs[0].id != old.id || refs[0].state != "expired" || refs[1].state != "open" || refs[1].amount != 1200 {
		t.Fatalf("refs = %+v", refs)
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE ref_id = ?`, old.id); n != 1 {
		t.Fatalf("the old ref keeps %d keys", n)
	}
	before := len(p.posts())
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
		t.Fatal("the older entry was given a link")
	}
	if len(p.posts()) != before || p.count(`SELECT count(*) FROM integration_ref_attempts WHERE ref_id = ?`, old.id) != 1 {
		t.Fatalf("the older entry took a new key: %d calls", len(p.posts())-before)
	}
}

func TestAKeyInDoubtLocksTheRecordsMoney(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-doubt-00001")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	p.exec(`UPDATE app_effects SET state = 'failed'`)
	mayBePaid := func() bool {
		t.Helper()
		yes, _, err := p.svc.MayBePaid(context.Background(), p.db, p.app.ID, record)
		if err != nil {
			t.Fatal(err)
		}
		return yes
	}
	for _, tc := range []struct {
		name, set string
		want      bool
	}{
		{"its outcome unknown", `UPDATE integration_ref_attempts SET outcome = NULL`, true},
		{"an object seen, unchecked", `UPDATE integration_ref_attempts SET object_id = 'cs_test_seen'`, true},
		{"confirmed away with an object seen", `UPDATE integration_ref_attempts SET outcome = 'abandoned'`, true},
		{"confirmed away", `UPDATE integration_ref_attempts SET object_id = NULL`, false},
		{"seen expired", `UPDATE integration_ref_attempts SET outcome = 'expired'`, false},
		{"nothing ran", `UPDATE integration_ref_attempts SET outcome = NULL, clean_sends = sends`, false},
	} {
		p.exec(tc.set)
		if got := mayBePaid(); got != tc.want {
			t.Errorf("%s: may be paid %v, want %v", tc.name, got, tc.want)
		}
	}
	p.exec(`UPDATE integration_ref_attempts SET clean_sends = 0`)
	expectCode(t, p.actRec("", "send", map[string]any{"id": record, "total": 12}), http.StatusUnprocessableEntity, string(apierr.CodeAppPaymentLinkOpen))
}

func (p *pay) republish(definition string) {
	p.t.Helper()
	rec := p.owner.do(http.MethodGet, p.appsPath(fmt.Sprintf("/%d", p.app.ID)), nil)
	expect(p.t, rec, http.StatusOK)
	app := decode[apps.App](p.t, rec)
	rec = p.owner.do(http.MethodPatch, p.appsPath(fmt.Sprintf("/%d", p.app.ID)), map[string]any{"version": app.Version, "definition": rawJSON(definition)})
	expect(p.t, rec, http.StatusOK)
	app = decode[apps.App](p.t, rec)
	rec = p.owner.do(http.MethodGet, p.appsPath(fmt.Sprintf("/%d/integrations", app.ID)), nil)
	expect(p.t, rec, http.StatusOK)
	hash := decode[apps.IntegrationStatus](p.t, rec).Slots[0].ContractHash
	rec = p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/integrations/payments/request", app.ID)), map[string]any{"contract_hash": hash})
	expect(p.t, rec, http.StatusOK)
	approval := decode[apps.IntegrationApproval](p.t, rec)
	expect(p.t, p.owner.do(http.MethodPost, wsPath(p.ws, fmt.Sprintf("integration-approvals/%d/approve", approval.ID)),
		map[string]any{"integration_id": p.in.ID}), http.StatusOK)
	expect(p.t, p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/publish", p.app.ID)),
		map[string]any{"version": app.Version, "acknowledge": []any{}}), http.StatusOK)
}

func TestTheMoneyStaysLockedAfterTheEffectIsRemoved(t *testing.T) {
	p := newPay(t, publicURL)
	record, r := p.sentInvoice(10, "EUR", "op-removed-001")
	p.republish(strings.Replace(invoiceApp, `"send": {"kind": "update", "effects": [{
      "kind": "create_payment_link", "integration": "payments", "amount_field": "total", "currency_field": "currency",
      "description": "Invoice {{number}} for {{customer}}", "url_field": "pay_url", "id_field": "payment_id"}]}`, `"send": {"kind": "update"}`, 1))
	expectCode(t, p.actRec("", "send", map[string]any{"id": record, "total": 12}), http.StatusUnprocessableEntity, string(apierr.CodeAppPaymentLinkOpen))
	expectCode(t, p.actRec("", "send", map[string]any{"id": record, "currency": "JPY"}), http.StatusUnprocessableEntity, string(apierr.CodeAppPaymentLinkOpen))
	p.expires(r.object)
	expect(t, p.actRec("", "send", map[string]any{"id": record, "total": 12}), http.StatusOK)
}

func TestAnOlderEntryNeverMovesALinksSlotBack(t *testing.T) {
	p := newPay(t, publicURL)
	record, r := p.sentInvoice(10, "EUR", "op-order-00001")
	p.stop()
	p.act("op-order-00002", "send", map[string]any{"id": record, "status": "sent", "customer": "B"})
	p.act("op-order-00003", "send", map[string]any{"id": record, "status": "sent", "customer": "C"})
	es := p.entries()
	req := func(effect int64, slot string) {
		t.Helper()
		_, err := p.svc.OpenPaymentLink(context.Background(), paylinkRequest(p, record, effect, slot))
		if err != nil {
			t.Fatalf("entry %d: %v", effect, err)
		}
	}
	p.exec(`UPDATE integration_refs SET slot = 'old_payments' WHERE id = ?`, r.id)
	req(es[2].id, "payments")
	req(es[1].id, "old_payments")
	var slot string
	if err := p.db.QueryRow(`SELECT slot FROM integration_refs WHERE id = ?`, r.id).Scan(&slot); err != nil || slot != "payments" {
		t.Fatalf("the ref's slot is %q, %v", slot, err)
	}
}

func TestAPaidRefsExpiredLinkIsKept(t *testing.T) {
	for _, how := range []string{"told by Stripe", "read by an entry"} {
		t.Run(how, func(t *testing.T) {
			p := newPay(t, publicURL)
			record, r := p.sentInvoice(10, "EUR", "op-paid-exp-01")
			p.exec(`UPDATE integration_refs SET state = 'paid', paid_object_id = 'cs_test_abandoned', payment_id = 'pi_test_1', settled_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, r.id)
			if how == "told by Stripe" {
				p.expires(r.object)
				if yes, _, err := p.svc.MayBePaid(context.Background(), p.db, p.app.ID, record); err != nil || yes {
					t.Fatalf("may be paid %v, %v", yes, err)
				}
			} else {
				if _, err := p.fake.Expire(r.object); err != nil {
					t.Fatal(err)
				}
				p.act("op-paid-exp-02", "send", map[string]any{"id": record, "status": "sent", "customer": "Other"})
				if es := p.allEnded(2); es[1].state != apps.StateDone || len(p.posts()) != 2 {
					t.Fatalf("the second entry: %+v, %d links", es[1], len(p.posts()))
				}
			}
			if n := p.count(`SELECT count(*) FROM integration_refs WHERE id = ? AND link_expired_at IS NOT NULL`, r.id); n != 1 {
				t.Fatal("the link's expiry wasn't kept")
			}
			var state, paidObject, settled string
			if err := p.db.QueryRow(`SELECT state, paid_object_id, settled_at FROM integration_refs WHERE id = ?`, r.id).Scan(&state, &paidObject, &settled); err != nil ||
				state != "paid" || paidObject != "cs_test_abandoned" || settled != "2026-01-01T00:00:00.000Z" {
				t.Fatalf("the payment changed: %s %s %s, %v", state, paidObject, settled, err)
			}
		})
	}
}

func TestALinkIsGivenAgainOnlyForItsOwnMoneyFields(t *testing.T) {
	p := newPay(t, publicURL)
	record, r := p.sentInvoice(10, "EUR", "op-fields-0001")
	p.stop()
	p.act("op-fields-0002", "send", map[string]any{"id": record, "status": "sent", "customer": "B"})
	req := paylinkRequest(p, record, p.entries()[1].id, "payments")
	req.AmountField = "alternate_total"
	_, err := p.svc.OpenPaymentLink(context.Background(), req)
	var pe *paylink.Error
	if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationLinkStillOpen {
		t.Fatalf("err = %v", err)
	}
	if refs := p.refs(); len(refs) != 1 || refs[0].id != r.id || len(p.posts()) != 1 {
		t.Fatalf("refs = %+v, %d links", refs, len(p.posts()))
	}
	if link, err := p.svc.OpenPaymentLink(context.Background(), paylinkRequest(p, record, p.entries()[1].id, "payments")); err != nil || link.ObjectID != r.object {
		t.Fatalf("link = %+v, %v", link, err)
	}
}
