package integrations

import (
	"context"
	"net/http"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/integrations/testprovider"
)

func TestAnExpiryEventOfAnotherObjectNeverMovesAnUnresolvedKeyOn(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-event-key-0001")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	first := p.fake.Sessions()[0].ID
	second := p.secondSessionOfToken(first)

	p.expires(second)

	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE outcome = 'expired'`); n != 0 {
		t.Fatalf("%d keys moved on by the expiry of one object while %s may be paid", n, first)
	}
	link, err := p.svc.OpenPaymentLink(ctx, req)
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if n := len(p.fake.Sessions()); n != 2 || link.ObjectID != first {
		t.Fatalf("link = %+v, %d sessions: a new payable link was made beside %s", link, n, first)
	}
	if s, _ := p.fake.Session(first); s.Status != "open" {
		t.Fatalf("session %s = %s", first, s.Status)
	}
}

func TestAnExpiryEventOfAnotherObjectMovesAnUnresolvedKeyOnOnceNothingMayBePaid(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-event-key-0002")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	first := p.fake.Sessions()[0].ID
	second := p.secondSessionOfToken(first)
	if _, err := p.fake.Expire(first); err != nil {
		t.Fatal(err)
	}
	p.expires(second)
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE outcome = 'expired'`); n != 1 {
		t.Fatalf("%d keys moved on, want 1: every session of the token ended", n)
	}
}

func TestASavedLinksExpiryEventKeepsTheMoneyLockedWhileAnotherObjectOfTheTokenMayBePaid(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-event-money-0001")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	first := p.fake.Sessions()[0].ID
	second := p.secondSessionOfToken(first)
	if _, err := p.svc.OpenPaymentLink(ctx, req); err != nil {
		t.Fatalf("the retry found no link: %v", err)
	}
	refs := p.refs()
	if len(refs) != 1 {
		t.Fatalf("refs = %+v", refs)
	}
	saved, other := refs[0].object, first
	if saved == first {
		other = second
	}
	p.exec(`UPDATE app_effects SET state = ?`, apps.StateDone)

	p.expires(saved)

	if refs := p.refs(); len(refs) != 1 || refs[0].state == "expired" {
		t.Fatalf("refs = %+v: the reference ended while %s may be paid", refs, other)
	}
	expectCode(t, p.actRec("", "send", map[string]any{"id": record, "total": 11}), http.StatusUnprocessableEntity, string(apierr.CodeAppPaymentLinkOpen))
	if p.field(record, "total") != float64(10) {
		t.Fatalf("the invoice's total changed to %v while %s may be paid", p.field(record, "total"), other)
	}

	if _, err := p.fake.Expire(other); err != nil {
		t.Fatal(err)
	}
	p.later(time.Hour)
	p.drain()
	if refs := p.refs(); len(refs) != 1 || refs[0].state != "expired" {
		t.Fatalf("refs = %+v: the reference never ended once every object had", refs)
	}
	expect(t, p.actRec("", "send", map[string]any{"id": record, "total": 11}), http.StatusOK)
}

func TestAnExpiryEventIsNeverProvenAgainstAnotherAccount(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-event-account-0001")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	first := p.fake.Sessions()[0].ID
	second := p.secondSessionOfToken(first)
	p.fake.SetAccount("acct_" + "Other" + "Account1")
	p.expires(second)
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE outcome = 'expired'`); n != 0 {
		t.Fatalf("%d keys moved on by a proof read from another account", n)
	}
}
