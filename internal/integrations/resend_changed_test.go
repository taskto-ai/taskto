package integrations

import (
	"net/http"
	"testing"
)

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
