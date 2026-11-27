package integrations

import (
	"strings"
	"testing"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/apps"
)

// invoiceAppWhen is the invoicing app whose link asks that the invoice be sent.
var invoiceAppWhen = strings.Replace(invoiceApp, `"kind": "create_payment_link", "integration": "payments",`,
	`"kind": "create_payment_link", "integration": "payments", "when": {"field": "status", "op": "eq", "value": "sent"},`, 1)

// The invoice is taken back to a draft while Stripe makes its link: the link exists at Stripe, and
// is not written to the record, so the entry says it was made and kept (not that nothing was). The
// reference keeps it, so a payment on it is never lost: it settles the reference, and the admins are
// told that the app's rule left the record as it was.
func TestALinkMadeWhileTheRecordStoppedQualifyingKeepsItsPayment(t *testing.T) {
	if invoiceAppWhen == invoiceApp {
		t.Fatal("the invoicing app has no payment link effect to add a rule to")
	}
	p := newPayWith(t, publicURL, invoiceAppWhen)
	record := p.invoice(10, "EUR")
	p.svc.afterCall = func() error {
		p.svc.afterCall = nil
		p.exec(`UPDATE app_rows SET data = json_set(data, '$.status', 'draft') WHERE id = ?`, record)
		return nil
	}
	p.startWorker()
	p.send(record, "op-kept-0001")
	e := p.settled(apps.StateFailed)
	if e.code != string(apierr.CodeIntegrationConditionChangedLinkKept) {
		t.Fatalf("entry = %+v", e)
	}
	refs := p.refs()
	if len(refs) != 1 || refs[0].object == "" || refs[0].state != "open" {
		t.Fatalf("refs = %+v: the link isn't kept with the entry", refs)
	}
	if data, _ := p.record(record); strings.Contains(data, refs[0].object) {
		t.Fatalf("record = %s", data)
	}

	p.pays(refs[0].object)
	if r := p.refOf(record); r.state != "paid" {
		t.Fatalf("ref = %+v: the payment on the kept link was lost", r)
	}
	p.mustEvent(p.lastEvent(EventPaymentSucceeded), EventIgnored, apierr.CodeIntegrationEventConditionUnmet)
	if all, _ := p.adminNotices(); all != 1 {
		t.Fatalf("%d notifications, want the admins told of the payment", all)
	}
}
