package integrations

import (
	"testing"
)

// A payer pays a live link, and before Stripe's event arrives an admin replaces the integration's key
// with a test key (refused now while the link may be paid, as a server before allowed it). The signed event is of live money for a ref made in live mode: it is stored and
// applied to that ref, never acknowledged and dropped because the integration's mode is another now.
func TestALivePaymentToldAfterTheKeyMovedToTestModeIsStillApplied(t *testing.T) {
	p := newPay(t, publicURL)
	p.setKey(stripeLiveKey)
	record, ref := p.sentInvoice(100, "EUR", "op-live-0001")
	evs, err := p.fake.Pay(ref.object)
	if err != nil {
		t.Fatal(err)
	}
	p.setKeyUnchecked(stripeTestKey) // refused now while the link may be paid; a server before let it
	p.deliver(p.sender(p.hookID(), stripeHook), evs)
	p.drain()
	if n := p.count(`SELECT count(*) FROM integration_events WHERE kind = ?`, EventPaymentSucceeded); n != 1 {
		t.Fatalf("%d payment events stored: the live payment was dropped", n)
	}
	if r := p.refOf(record); r.state != "paid" {
		t.Fatalf("ref = %+v: the payment was lost", r)
	}
}
