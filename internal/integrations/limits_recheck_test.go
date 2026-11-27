package integrations

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/integrations/testprovider"
	"taskto.ai/taskto/internal/paylink"
)

// The admin's limits are read again right before a call under a new key leaves, not only when the try
// began: a try that goes from a key whose object expired to the next one asks for a new link, and the
// ceiling an admin lowered meanwhile refuses it. Nothing is sent under the new key.
func TestTheLimitsAreReadAgainBeforeANewKeysCall(t *testing.T) {
	for name, change := range map[string]string{
		"the ceiling lowered":  `UPDATE integrations SET max_amount_minor = 500 WHERE id = ?`,
		"the currency removed": `UPDATE integrations SET currencies = '["USD"]' WHERE id = ?`,
	} {
		t.Run(name, func(t *testing.T) {
			p := newPay(t, publicURL)
			record := p.invoice(10, "EUR")
			req := p.direct(record, "op-limits-0001")
			p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
			if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
				t.Fatal("the lost answer was no error")
			}
			if _, err := p.fake.Expire(p.fake.Sessions()[0].ID); err != nil {
				t.Fatal(err)
			}
			// The lookup can't be read, so the key is asked again, and Stripe answers with the session it
			// kept, which has expired: the try moves on to the next key. The admin changes the limits
			// meanwhile.
			p.fake.Fail(testprovider.Unavailable().On(http.MethodGet, "/v1/checkout/sessions"))
			// The change commits once the replay has left, before the next key's call is written ahead
			// (one after the write-ahead is TestALimitChangedBeforeTheCallLeavesStopsIt's).
			p.svc.beforeSettle = func(string) {
				p.svc.beforeSettle = nil
				p.exec(change, p.in.ID)
			}
			_, err := p.svc.OpenPaymentLink(context.Background(), req)
			var pe *paylink.Error
			if !errors.As(err, &pe) || (pe.Code != apierr.CodeIntegrationAmountAboveMax && pe.Code != apierr.CodeIntegrationCurrencyRefused) {
				t.Fatalf("err = %v: want the limits' refusal", err)
			}
			if posts := p.posts(); len(posts) != 2 {
				t.Fatalf("%d calls: want the first and its replay, none under a new key", len(posts))
			}
			if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE seq > 0 AND sends > 0`); n != 0 {
				t.Fatalf("%d calls written ahead under a new key", n)
			}
		})
	}
}

// Limits an admin lowered while a key's outcome is unknown don't stop the lookup of what its call may
// have made: the object that turns up is the link (one link, and its payment is recorded), and nothing is
// asked under the key or a new one past the limits.
func TestLoweredLimitsNeverStopTheLookupOfAnUnknownKey(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-limits-0002")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	p.exec(`UPDATE integrations SET max_amount_minor = 500 WHERE id = ?`, p.in.ID)
	link, err := p.svc.OpenPaymentLink(context.Background(), req)
	if err != nil {
		t.Fatalf("err = %v: the lookup of the unknown key was stopped by the limits", err)
	}
	if link.ObjectID != p.fake.Sessions()[0].ID {
		t.Fatalf("link = %+v", link)
	}
	if posts := p.posts(); len(posts) != 1 {
		t.Fatalf("%d calls: want the first only", len(posts))
	}

	// With nothing to find, the key is not asked again past the limits: the entry waits on the lookup.
	p = newPay(t, publicURL)
	req = p.direct(p.invoice(10, "EUR"), "op-limits-0003")
	p.fake.Fail(testprovider.ServerError().KeptUnderKey().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
		t.Fatal("the 500 was no error")
	}
	p.exec(`UPDATE integrations SET max_amount_minor = 500 WHERE id = ?`, p.in.ID)
	_, err = p.svc.OpenPaymentLink(context.Background(), req)
	var pe *paylink.Error
	if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationLinkLookupPending {
		t.Fatalf("err = %v: want the wait on the lookup", err)
	}
	if posts := p.posts(); len(posts) != 1 {
		t.Fatalf("%d calls: the key was asked again past the limits", len(posts))
	}
}
