package integrations

import (
	"testing"
)

func TestALivePaymentToldAfterTheKeyMovedToTestModeIsStillApplied(t *testing.T) {
	p := newPay(t, publicURL)
	p.setKey(stripeLiveKey)
	record, ref := p.sentInvoice(100, "EUR", "op-live-0001")
	evs, err := p.fake.Pay(ref.object)
	if err != nil {
		t.Fatal(err)
	}
	p.setKeyUnchecked(stripeTestKey)
	p.deliver(p.sender(p.hookID(), stripeHook), evs)
	p.drain()
	if n := p.count(`SELECT count(*) FROM integration_events WHERE kind = ?`, EventPaymentSucceeded); n != 1 {
		t.Fatalf("%d payment events stored: the live payment was dropped", n)
	}
	if r := p.refOf(record); r.state != "paid" {
		t.Fatalf("ref = %+v: the payment was lost", r)
	}
}
