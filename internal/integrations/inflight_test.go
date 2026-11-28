package integrations

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/integrations/testprovider"
)

var invoiceAppWhen = strings.Replace(invoiceApp, `"kind": "create_payment_link", "integration": "payments",`,
	`"kind": "create_payment_link", "integration": "payments", "when": {"field": "status", "op": "eq", "value": "sent"},`, 1)

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

func TestAConditionBrokenWhileTheOutcomeIsUnknownKeepsLookingForTheLink(t *testing.T) {
	p := newPayWith(t, publicURL, invoiceAppWhen)
	record := p.invoice(10, "EUR")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	p.startWorker()
	p.send(record, "op-kept-0002")
	p.waitsUnknown()
	p.exec(`UPDATE app_rows SET data = json_set(data, '$.status', 'draft') WHERE id = ?`, record)
	p.later(time.Minute)
	e := p.settled(apps.StateFailed)
	if e.code != string(apierr.CodeIntegrationConditionChangedLinkKept) {
		t.Fatalf("entry = %+v: want the made link reported kept", e)
	}
	refs := p.refs()
	if len(refs) != 1 || refs[0].object != p.fake.Sessions()[0].ID {
		t.Fatalf("refs = %+v", refs)
	}
	if n := p.objects("/v1/checkout/sessions"); n != 1 {
		t.Fatalf("%d sessions", n)
	}
}
