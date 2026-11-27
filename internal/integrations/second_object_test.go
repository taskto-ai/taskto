package integrations

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/integrations/testprovider"
	"taskto.ai/taskto/internal/paylink"
)

// The key has an object on record, seen and not checked (A, open at Stripe). A lookup lists another
// session of the key's token (B), which expires before its state is read. B never takes A's place on
// the key, and its expiry never moves the ref on: A may be paid. The entry waits, nothing is made.
func TestAnotherSessionOfTheKeyNeverReplacesTheOneOnRecord(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-two-0001")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	a := p.fake.Sessions()[0]
	p.exec(`UPDATE integration_ref_attempts SET object_id = ?, object_verified = 0 WHERE seq = 0`, a.ID)
	b := url.Values{}
	b.Set("mode", "payment")
	b.Set("success_url", publicURL+PaymentDonePath)
	b.Set("client_reference_id", *a.ClientReferenceID)
	b.Set("line_items[0][quantity]", "1")
	b.Set("line_items[0][price_data][currency]", "eur")
	b.Set("line_items[0][price_data][unit_amount]", "1000")
	b.Set("line_items[0][price_data][product_data][name]", "Invoice")
	p.mustPost("/v1/checkout/sessions", b)
	other := p.fake.Sessions()[len(p.fake.Sessions())-1].ID
	p.svc.beforeSettle = func(objectID string) {
		if objectID == other {
			if _, err := p.fake.Expire(other); err != nil {
				t.Error(err)
			}
		}
	}
	posts := len(p.posts())
	_, err := p.svc.OpenPaymentLink(ctx, req)
	var pe *paylink.Error
	if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationLinkLookupPending {
		t.Fatalf("err = %v: want the wait", err)
	}
	if len(p.posts()) != posts {
		t.Fatal("a call went out while the session on record may be paid")
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE seq = 0 AND object_id = ? AND outcome IS NULL`, a.ID); n != 1 {
		t.Fatal("the session on record was replaced or the key given an outcome")
	}
	if r := p.refs(); r[0].keySeq != 0 {
		t.Fatalf("refs = %+v: the ref moved on", r)
	}
}
