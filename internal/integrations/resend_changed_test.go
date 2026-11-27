package integrations

import (
	"net/http"
	"testing"
)

// An invoice of 10.00 is sent, and while its session is open the invoice is sent again with its total
// changed to 11.00. The record keeps one payable link: the change of its total is refused while the
// link may be paid (app_payment_link_open), so the second send never asks for a second session.
func TestAnInvoiceSentAgainWithAnotherTotalKeepsOnePayableLink(t *testing.T) {
	p := newPay(t, publicURL)
	record, ref := p.sentInvoice(10, "EUR", "op-resend-0001")
	rec := p.actRec("op-resend-0002", "send", map[string]any{"id": record, "status": "sent", "total": 11})
	if rec.Code == http.StatusOK {
		p.waitFor("the second entry", func() bool {
			es := p.entries()
			return len(es) == 2 && es[1].state != "pending" && es[1].state != "running"
		})
	}
	open := 0
	for _, s := range p.fake.Sessions() {
		if s.Status == "open" {
			open++
		}
	}
	if open != 1 {
		t.Fatalf("%d open sessions for one invoice (answer %d)", open, rec.Code)
	}
	if r := p.refOf(record); r.object != ref.object {
		t.Fatalf("ref = %+v, was %+v", r, ref)
	}
}
