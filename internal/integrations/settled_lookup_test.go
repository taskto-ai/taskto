package integrations

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/integrations/testprovider"
)

// A lookup found the key's session completed (paid) before the payment's event arrived, and the entry
// stopped for a person. The invoice is put back to a draft and the entry retried: it still says the
// link was paid, never that nothing was created.
func TestARetryAfterASettledLookupNeverSaysNothingWasMade(t *testing.T) {
	p := newPayWith(t, publicURL, invoiceAppWhen)
	record := p.invoice(10, "EUR")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	p.startWorker()
	p.send(record, "op-paid-0001")
	p.waitsUnknown()
	if _, err := p.fake.Pay(p.fake.Sessions()[0].ID); err != nil {
		t.Fatal(err)
	}
	p.later(time.Minute)
	e := p.settled(apps.StateFailed)
	if e.code != string(apierr.CodeIntegrationLinkAlreadyPaid) {
		t.Fatalf("entry = %+v", e)
	}
	p.exec(`UPDATE app_rows SET data = json_set(data, '$.status', 'draft') WHERE id = ?`, record)
	expect(t, p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/effects/%d/retry", p.app.ID, e.id)), map[string]any{}), http.StatusOK)
	p.later(time.Minute)
	if e = p.settled(apps.StateFailed); e.code != string(apierr.CodeIntegrationLinkAlreadyPaid) {
		t.Fatalf("entry = %+v: a paid link was reported as none made", e)
	}
}
