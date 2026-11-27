package integrations

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/integrations/testprovider"
)

// The invoice's link, made under one release, and the releases after it: whatever field, slot or
// integration a later release asks for the link with, the invoice has one payable link, its money
// stays while the link may be paid, and its payment is written.

// definitionWith is invoiceApp with the slot named slot and, if alternateURL, the link written to
// another field.
func definitionWith(t *testing.T, slot string, alternateURL bool) string {
	t.Helper()
	out := invoiceApp
	if alternateURL {
		out = strings.Replace(out, `{"name": "pay_url",`, `{"name": "new_pay_url", "type": "string", "max_length": 2048}, {"name": "pay_url",`, 1)
		out = strings.Replace(out, `"url_field": "pay_url"`, `"url_field": "new_pay_url"`, 1)
	}
	if slot != "payments" {
		out = strings.Replace(out, `"integration": "payments"`, `"integration": "`+slot+`"`, 1)
		out = strings.Replace(out, `"integrations": {"payments":`, `"integrations": {"`+slot+`":`, 1)
	}
	return out
}

// publishApproved saves definition as the draft, has the owner approve its slot with integration, and
// publishes it.
func publishApproved(p *pay, definition, slot string, integration int64, acknowledge []any) *httptest.ResponseRecorder {
	p.t.Helper()
	path := p.appsPath(fmt.Sprintf("/%d", p.app.ID))
	rec := p.owner.do(http.MethodGet, path, nil)
	expect(p.t, rec, http.StatusOK)
	app := decode[apps.App](p.t, rec)
	rec = p.owner.do(http.MethodPatch, path, map[string]any{"version": app.Version, "definition": rawJSON(definition)})
	expect(p.t, rec, http.StatusOK)
	app = decode[apps.App](p.t, rec)
	rec = p.owner.do(http.MethodGet, path+"/integrations", nil)
	expect(p.t, rec, http.StatusOK)
	hash := decode[apps.IntegrationStatus](p.t, rec).Slots[0].ContractHash
	rec = p.owner.do(http.MethodPost, path+"/integrations/"+slot+"/request", map[string]any{"contract_hash": hash})
	expect(p.t, rec, http.StatusOK)
	approval := decode[apps.IntegrationApproval](p.t, rec)
	expect(p.t, p.owner.do(http.MethodPost, wsPath(p.ws, fmt.Sprintf("integration-approvals/%d/approve", approval.ID)),
		map[string]any{"integration_id": integration, "allow_space": true}), http.StatusOK)
	return p.owner.do(http.MethodPost, path+"/publish", map[string]any{"version": app.Version, "acknowledge": acknowledge})
}

// A release that writes the link to another field, or a slot bound to another integration since, asks
// for the invoice's link again: its open link is the one, and nothing else is made.
func TestALinkWrittenToAnotherFieldIsTheInvoicesSameLink(t *testing.T) {
	for _, across := range []bool{false, true} {
		t.Run(fmt.Sprintf("account-change-%v", across), func(t *testing.T) {
			p := newPay(t, publicURL)
			id, original := p.sentInvoice(10, "EUR", "send-original")
			integration := p.in.ID
			if across {
				body := stripeBody("other")
				body["secrets"] = map[string]string{"api_key": p.fake.APIKey(), "webhook_secret": hookFor("send-other")}
				other := p.create(p.owner, p.ws, body)
				integration = other.ID
				p.exec(`INSERT INTO integration_spaces (integration_id, space_id) VALUES (?, ?)`, integration, p.space.ID)
			}
			expect(t, publishApproved(p, definitionWith(t, "payments", true), "payments", integration, nil), http.StatusOK)
			p.act("send-new-url", "send", map[string]any{"id": id, "customer": "Other"})
			entries := p.allEnded(2)
			if n := len(p.fake.Sessions()); n != 1 {
				t.Fatalf("%d sessions (original %s, old field %v and new field %v), second entry %+v", n, original.object, p.field(id, "pay_url"), p.field(id, "new_pay_url"), entries[1])
			}
		})
	}
}

// A payment of a link made before an approved release renamed its slot is written through the slot
// the live release binds to the same integration, though no entry gave the link again.
func TestAPaymentAfterASlotRenameIsWritten(t *testing.T) {
	p := newPay(t, publicURL)
	id, original := p.sentInvoice(10, "EUR", "send-original")
	expect(t, publishApproved(p, definitionWith(t, "billing", false), "billing", p.in.ID, nil), http.StatusOK)
	p.pays(original.object)
	if v := p.field(id, "status"); v != "paid" {
		t.Fatalf("status = %v, events = %+v", v, p.eventRows())
	}
}

// A release can't read the money of an invoice whose link may be paid otherwise than the live one
// does, however its change was acknowledged.
func TestAReleaseCantReadAnOpenLinksMoneyOtherwise(t *testing.T) {
	p := newPay(t, publicURL)
	p.sentInvoice(10, "EUR", "send-original")
	def := strings.Replace(invoiceApp, `"name": "total", "type": "number"`, `"name": "total", "type": "string"`, 1)
	def = strings.Replace(def, `{"name": "paid_at"`, `{"name": "new_total", "type": "number"}, {"name": "paid_at"`, 1)
	def = strings.Replace(def, `"amount_field": "total"`, `"amount_field": "new_total"`, 1)
	response := publishApproved(p, def, "payments", p.in.ID, []any{map[string]any{"change": "field_type_changed", "field": "total"}})
	expectCode(t, response, http.StatusUnprocessableEntity, string(apierr.CodeAppPaymentLinkOpen))
}

// A verified expiry of the session of a key whose answer was lost resolves that key, and the money is
// free again.
func TestASignedExpiryOfAKeyInDoubtFreesTheMoney(t *testing.T) {
	p := newPay(t, publicURL)
	id := p.invoice(10, "EUR")
	req := p.direct(id, "send-unknown")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
		t.Fatal("expected lost response")
	}
	p.exec(`UPDATE app_effects SET state = 'failed', error_code = ?`, string(apierr.CodeIntegrationOutcomeUnknown))
	p.expires(p.fake.Sessions()[0].ID)
	response := p.actRec("", "send", map[string]any{"id": id, "total": 12})
	if response.Code != http.StatusOK {
		t.Fatalf("edit returned %d %s, events %+v", response.Code, response.Body, p.eventRows())
	}
}

// A ref made before refs kept their money's fields, whose entry was cleaned up, locks every field a
// release of the app read money from, even once the live release asks for no link.
func TestAnOldRefWithoutItsFieldsLocksEveryMoneyField(t *testing.T) {
	p := newPay(t, publicURL)
	id, original := p.sentInvoice(10, "EUR", "send-original")
	// The outbox purge left effect_id NULL before 0063 ran: these are the fields 0063 gives that row.
	p.exec(`UPDATE integration_refs SET effect_id = NULL, amount_field = '', currency_field = '' WHERE id = ?`, original.id)
	p.exec(`DELETE FROM app_effects`)
	p.republish(strings.Replace(invoiceApp, `"send": {"kind": "update", "effects": [{
      "kind": "create_payment_link", "integration": "payments", "amount_field": "total", "currency_field": "currency",
      "description": "Invoice {{number}} for {{customer}}", "url_field": "pay_url", "id_field": "payment_id"}]}`, `"send": {"kind": "update"}`, 1))
	expectCode(t, p.actRec("", "send", map[string]any{"id": id, "total": 12}), http.StatusUnprocessableEntity, string(apierr.CodeAppPaymentLinkOpen))
}

// secondPayableLink gives the record a second ref with a link a payer may use beside its first, as the
// writer before refs were per record could: the first is lent to another record while the second is
// made, and then given back.
func (p *pay) secondPayableLink(id int64, first refRow, op string) {
	p.t.Helper()
	other := p.invoice(10, "EUR")
	p.exec(`UPDATE integration_refs SET record_id = ? WHERE id = ?`, other, first.id)
	p.act(op, "send", map[string]any{"id": id, "customer": "Second"})
	p.allEnded(2)
	p.exec(`UPDATE integration_refs SET record_id = ? WHERE id = ?`, id, first.id)
	if refs := p.refs(); len(refs) != 2 || len(p.fake.Sessions()) != 2 {
		p.t.Fatalf("the fixture needs two payable sessions: refs %+v", refs)
	}
}

// Every ref of the record that may be paid counts, not only the newest: an older one still open is
// given again, and nothing new is made.
func TestEveryPayableRefOfTheRecordCounts(t *testing.T) {
	p := newPay(t, publicURL)
	id, _ := p.sentInvoice(10, "EUR", "send-original")
	p.secondPayableLink(id, p.refs()[0], "send-second")
	refs := p.refs()
	p.expires(refs[1].object)
	p.act("send-third", "send", map[string]any{"id": id, "customer": "Third"})
	entries := p.allEnded(3)
	if n := len(p.fake.Sessions()); n != 2 {
		t.Fatalf("made session %d even though original %s remains payable, third entry %+v", n, refs[0].object, entries[2])
	}
}

// An event's rule (a system write) can't change the money while another link of the record may be
// paid.
func TestAnEventsRuleCantChangeMoneyWhileALinkMayBePaid(t *testing.T) {
	p := newPay(t, publicURL)
	id, original := p.sentInvoice(10, "EUR", "send-original")
	def := strings.Replace(invoiceApp, `"payment.succeeded":`, `"payment.expired": {"set": {"total": {"value": 12}}}, "payment.succeeded":`, 1)
	expect(t, publishApproved(p, def, "payments", p.in.ID, nil), http.StatusOK)
	p.secondPayableLink(id, p.refs()[0], "send-second")
	p.expires(original.object)
	if amount := p.field(id, "total"); amount != float64(10) {
		t.Fatalf("expiry handler changed total to %v while newer session remains payable, events %+v", amount, p.eventRows())
	}
}

// The invoicing template records a payment whatever an ordinary edit made the status meanwhile.
func TestTheTemplateRecordsAPaymentWhateverTheStatus(t *testing.T) {
	p := newPayWith(t, publicURL, invoicingDefinition(t))
	id := p.templateInvoice(120)
	p.startWorker()
	p.act("send-original", "send_invoice", map[string]any{"id": id, "status": "sent"})
	p.allEnded(1)
	object := p.field(id, "payment_id").(string)
	expect(t, p.actRec("", "edit_invoice", map[string]any{"id": id, "status": "draft"}), http.StatusOK)
	p.pays(object)
	if status := p.field(id, "status"); status != "paid" || p.field(id, "paid_at") == nil {
		t.Fatalf("status = %v, paid_at = %v, events %+v", status, p.field(id, "paid_at"), p.eventRows())
	}
}

// A payment of an expired link that arrives after the amount changed is recorded, and the amount
// stays the new one.
func TestALatePaymentAppliesAfterTheAmountChanged(t *testing.T) {
	p := newPayWith(t, publicURL, invoicingDefinition(t))
	id := p.templateInvoice(120)
	p.startWorker()
	p.act("send-original", "send_invoice", map[string]any{"id": id, "status": "sent"})
	p.allEnded(1)
	old := p.refs()[0]
	p.expires(old.object)
	expect(t, p.actRec("", "edit_invoice", map[string]any{"id": id, "total": 130}), http.StatusOK)
	p.put(Event{Kind: EventPaymentSucceeded, RefToken: old.token, ObjectID: old.object, AmountMinor: old.amount, Currency: "EUR"})
	p.drain()
	if status := p.field(id, "status"); status != "paid" || p.field(id, "total") != float64(130) {
		t.Fatalf("status = %v, current total %v, events %+v", status, p.field(id, "total"), p.eventRows())
	}
}

// Two replacements of an expired link that run at once make one link, at the amount the last edit
// left.
func TestTwoReplacementsMakeOneLinkAtTheCurrentAmount(t *testing.T) {
	p := newPayWith(t, publicURL, invoicingDefinition(t))
	id := p.templateInvoice(120)
	p.startWorker()
	p.act("send-original", "send_invoice", map[string]any{"id": id, "status": "sent"})
	p.allEnded(1)
	original := p.refs()[0]
	p.expires(original.object)
	p.stop()
	startEdits := make(chan struct{})
	edits := make(chan *httptest.ResponseRecorder, 2)
	for _, total := range []int{130, 140} {
		go func() {
			<-startEdits
			edits <- p.actRec(fmt.Sprintf("send-edit-%d", total), "edit_invoice", map[string]any{"id": id, "total": total})
		}()
	}
	close(startEdits)
	for range 2 {
		expect(t, <-edits, http.StatusOK)
	}
	total := fmt.Sprint(p.field(id, "total"))
	startActions := make(chan struct{})
	actions := make(chan *httptest.ResponseRecorder, 2)
	for _, name := range []string{"send_invoice", "replace_link"} {
		go func() {
			<-startActions
			actions <- p.actRec("send-"+name, name, map[string]any{"id": id, "customer": name})
		}()
	}
	close(startActions)
	for range 2 {
		expect(t, <-actions, http.StatusOK)
	}
	entries := p.entries()
	if len(entries) != 3 {
		t.Fatalf("expected both replacements queued, entries %+v", entries)
	}

	var arrivals atomic.Int32
	proceed := make(chan struct{})
	arrived := make(chan struct{}, 2)
	p.svc.beforeBeginRef = func() {
		if arrivals.Add(1) <= 2 {
			arrived <- struct{}{}
			<-proceed
		}
	}
	done := make(chan error, 2)
	for _, entry := range entries[1:] {
		go func() {
			r := paylinkRequest(p, id, entry.id, "payments")
			r.Amount = total
			_, err := p.svc.OpenPaymentLink(context.Background(), r)
			done <- err
		}()
	}
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			close(proceed)
			t.Fatal("both opens did not reach the post-provider-read barrier")
		}
	}
	close(proceed)
	first, second := <-done, <-done
	refs := p.refs()
	if n := len(p.fake.Sessions()); n != 2 || len(refs) != 2 || fmt.Sprint(refs[1].amount/100) != total {
		t.Fatalf("after two opens (%v, %v) there are %d sessions and refs %+v for total %s", first, second, n, refs, total)
	}
}

// A payment is written to the field the live release names now.
func TestAPaymentWritesARenamedField(t *testing.T) {
	p := newPay(t, publicURL)
	id, original := p.sentInvoice(10, "EUR", "send-original")
	def := strings.ReplaceAll(invoiceApp, `"paid_at"`, `"received_at"`)
	expect(t, publishApproved(p, def, "payments", p.in.ID, nil), http.StatusOK)
	p.pays(original.object)
	if p.field(id, "status") != "paid" || p.field(id, "received_at") == nil {
		t.Fatalf("status %v, received_at %v, events %+v", p.field(id, "status"), p.field(id, "received_at"), p.eventRows())
	}
}

// An offline edit of the money, replayed while the link may be paid, is refused; once paid, it applies.
func TestAnOfflineEditOfTheMoneyWaitsForThePayment(t *testing.T) {
	p := newPayWith(t, publicURL, invoicingDefinition(t))
	id := p.templateInvoice(120)
	p.startWorker()
	p.act("send-original", "send_invoice", map[string]any{"id": id, "status": "sent"})
	p.allEnded(1)
	revision := p.revision(id)
	var release int64
	if err := p.db.QueryRow(`SELECT live_release FROM apps WHERE id = ?`, p.app.ID).Scan(&release); err != nil {
		t.Fatal(err)
	}
	body := apps.ClientAction{
		Name: "edit_invoice", SurfaceID: apps.SurfaceID(p.app.ID), SourceComponentID: "explorer", Context: map[string]any{"id": id, "total": 130},
		Release: &release, ExpectedRevision: &revision, Operation: "offline-edit-01",
	}
	path := p.appsPath(fmt.Sprintf("/%d/actions", p.app.ID))
	expectCode(t, p.owner.do(http.MethodPost, path, body), http.StatusUnprocessableEntity, string(apierr.CodeAppPaymentLinkOpen))
	first := p.field(id, "payment_id").(string)
	p.pays(first)
	body.ExpectedRevision = ptr(p.revision(id))
	expect(t, p.owner.do(http.MethodPost, path, body), http.StatusOK)
	if total := p.field(id, "total"); total != float64(130) {
		t.Fatalf("total after payment: %v", total)
	}
}

// A one-use Stripe Payment Link paid through its own Checkout Session is used up: its money is free.
func TestAPaidHostedLinkFreesItsMoney(t *testing.T) {
	p := newPayWith(t, "", invoicingDefinition(t))
	id := p.templateInvoice(120)
	p.startWorker()
	p.act("send-original", "send_invoice", map[string]any{"id": id, "status": "sent"})
	p.allEnded(1)
	original := p.refs()[0]
	events, err := p.fake.PayLink(original.object)
	if err != nil {
		t.Fatal(err)
	}
	p.svc.publicURL = publicURL // the server has now exposed the webhook for Stripe.
	var publicID string
	if err := p.db.QueryRow(`SELECT public_id FROM integrations WHERE id = ?`, p.in.ID).Scan(&publicID); err != nil {
		t.Fatal(err)
	}
	var completed testprovider.Event
	for _, event := range events {
		if event.Type == testprovider.EventSessionCompleted {
			var data map[string]any
			if err := event.ObjectAs(&data); err != nil {
				t.Fatal(err)
			}
			// The real payment URL has this parameter. The fake PayLink helper takes the bare id.
			data["client_reference_id"] = original.token
			completed = testprovider.NewEvent(event.Type, data, time.Now())
		}
	}
	if completed.ID == "" || p.fake.Links()[0].Active {
		t.Fatal("the fixture must complete and deactivate the one-payment link")
	}
	p.deliver(p.sender(publicID, stripeHook), []testprovider.Event{completed})
	p.drain()
	if got := p.field(id, "status"); got != "paid" {
		t.Fatalf("status = %v, events %+v", got, p.eventRows())
	}
	response := p.actRec("", "edit_invoice", map[string]any{"id": id, "total": 130})
	if response.Code != http.StatusOK {
		t.Fatalf("the paid invoice remains locked: status %d, code %s", response.Code, response.Body)
	}
}

// A try that read a ref before a newer entry retired it makes no key for it afterwards.
func TestARetiredRefTakesNoKey(t *testing.T) {
	p := newPay(t, publicURL)
	id := p.invoice(10, "EUR")
	req := p.direct(id, "send-unknown")
	ctx := context.Background()
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("expected lost response")
	}
	p.fake.ClearFaults()
	if _, err := p.fake.Expire(p.fake.Sessions()[0].ID); err != nil {
		t.Fatal(err)
	}
	p.exec(`UPDATE integration_ref_attempts SET outcome = 'expired'`)
	p.exec(`UPDATE app_effects SET state = 'failed', error_code = ?`, string(apierr.CodeIntegrationOutcomeUnknown))
	old, err := p.svc.refByID(ctx, p.refs()[0].id)
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.svc.attemptByKey(ctx, old.id, old.keySeq)
	if err != nil {
		t.Fatal(err)
	}
	// An older try successfully advanced the sequence, then paused before the next loop iteration.
	stale, err := p.svc.nextAttempt(ctx, old, first)
	if err != nil || stale.keySeq != old.keySeq+1 {
		t.Fatalf("nextAttempt = %+v, %v", stale, err)
	}
	p.act("send-replace", "send", map[string]any{"id": id, "total": 12})
	p.startWorker()
	p.allEnded(2)
	if refs := p.refs(); len(refs) != 2 || refs[0].state != "expired" || refs[1].amount != 1200 {
		t.Fatalf("refs after replacing = %+v", refs)
	}
	// Resuming OpenPaymentLink would call currentAttempt before the write-ahead guard.
	_, _ = p.svc.currentAttempt(ctx, stale)
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE ref_id = ?`, old.id); n != 1 {
		t.Fatalf("retired ref acquired a second key (%d rows); provider kept %d sessions", n, len(p.fake.Sessions()))
	}
}
