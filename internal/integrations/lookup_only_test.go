package integrations

import (
	"net/http"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/integrations/testprovider"
)

func TestARecordThatStoppedQualifyingIsOnlyLookedUpNeverSentFor(t *testing.T) {
	p := newPayWith(t, publicURL, invoiceAppWhen)
	record := p.invoice(10, "EUR")
	p.fake.Fail(testprovider.ConnectionReset().On(http.MethodPost, "/v1/checkout/sessions"))
	p.startWorker()
	p.send(record, "op-draft-0001")
	p.waitsUnknown()
	p.exec(`UPDATE app_rows SET data = json_set(data, '$.status', 'draft') WHERE id = ?`, record)
	p.later(time.Minute)
	p.looked("/v1/checkout/sessions", 1)
	if n := len(p.fake.RequestsTo(http.MethodPost, "/v1/checkout/sessions")); n != 1 {
		t.Fatalf("%d calls: a link was asked for while the record is a draft", n)
	}
	if n := p.objects("/v1/checkout/sessions"); n != 0 {
		t.Fatalf("%d sessions made for a draft", n)
	}
	p.later(7 * time.Hour)
	if e := p.settled(apps.StateFailed); e.code != string(apierr.CodeIntegrationOutcomeUnknown) {
		t.Fatalf("entry = %+v: want the admin's decision", e)
	}
	if n := len(p.fake.RequestsTo(http.MethodPost, "/v1/checkout/sessions")); n != 1 {
		t.Fatalf("%d calls", n)
	}
}
