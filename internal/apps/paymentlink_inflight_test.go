package apps

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/paylink"
)

// A record edited while the provider makes its link, so that it no longer meets the action's condition:
// the link exists at the provider, and the entry says so (it is kept with the entry, and a payment on it
// is still recorded), not that nothing was created. The record isn't written. Retried once the record
// meets the condition again, the entry writes that same link, and no other is made.
func TestAConditionBrokenWhileTheLinkIsMadeSaysTheLinkIsKept(t *testing.T) {
	p := newPay(t)
	p.release(`"amount_field": "total", "currency": "EUR",`, `"amount_field": "total", "currency": "EUR", "when": {"field": "status", "op": "eq", "value": "sent"},`)
	p.send(p.alice, "op-send-0001")
	_, rev, _ := p.row()
	p.links.onOpen = func(paylink.Request) {
		p.links.onOpen = nil
		p.exec(`UPDATE app_rows SET data = json_set(data, '$.status', 'draft') WHERE id = ?`, p.invoice)
	}
	p.work()
	e := p.entries(p.alice).Effects[0]
	if e.Status != StateFailed || e.Error == nil || e.Error.Code != apierr.CodeIntegrationConditionChangedLinkKept {
		t.Fatalf("entry = %+v: want it to say the link was made and kept", e)
	}
	if data, rev2, _ := p.row(); strings.Contains(data, "pay_url\":\"h") || rev2 != rev {
		t.Fatalf("the record was written: %s", data)
	}
	// Retried while the record still doesn't qualify: the same answer, and nothing asked of anyone.
	expect(t, p.alice.do(http.MethodPost, appPath(p.shared, p.app.ID, fmt.Sprintf("/effects/%d/retry", e.ID)), map[string]any{}), http.StatusOK)
	p.work()
	if e := p.entries(p.alice).Effects[0]; e.Error == nil || e.Error.Code != apierr.CodeIntegrationConditionChangedLinkKept || p.links.count() != 1 {
		t.Fatalf("entry = %+v, %d calls", e, p.links.count())
	}
	// The record qualifies again: a retry writes the link the entry made, the only one.
	p.exec(`UPDATE app_rows SET data = json_set(data, '$.status', 'sent') WHERE id = ?`, p.invoice)
	expect(t, p.alice.do(http.MethodPost, appPath(p.shared, p.app.ID, fmt.Sprintf("/effects/%d/retry", e.ID)), map[string]any{}), http.StatusOK)
	p.work()
	if e := p.entries(p.alice).Effects[0]; e.Status != StateDone {
		t.Fatalf("entry = %+v", e)
	}
	if len(p.links.made) != 1 {
		t.Fatalf("%d links made", len(p.links.made))
	}
	if data, _, _ := p.row(); !strings.Contains(data, p.links.made[e.ID].URL) {
		t.Fatalf("record = %s", data)
	}
}
