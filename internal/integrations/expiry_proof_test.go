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

func (p *pay) secondSessionOfToken(first string) string {
	p.t.Helper()
	s, _ := p.fake.Session(first)
	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("success_url", publicURL+PaymentDonePath)
	form.Set("client_reference_id", *s.ClientReferenceID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", "eur")
	form.Set("line_items[0][price_data][unit_amount]", "1000")
	form.Set("line_items[0][price_data][product_data][name]", "Invoice")
	p.mustPost("/v1/checkout/sessions", form)
	all := p.fake.Sessions()
	return all[len(all)-1].ID
}

func TestAReplacementWaitsWhileAnotherObjectOfTheSavedLinksTokenMayBePaid(t *testing.T) {
	for name, byEvent := range map[string]bool{"the saved link seen expired when asked": false, "the saved link ended by a signed event": true} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			p := newPay(t, publicURL)
			record := p.invoice(10, "EUR")
			req := p.direct(record, "op-replace-0001")
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
			if len(refs) != 1 || (refs[0].object != first && refs[0].object != second) {
				t.Fatalf("refs = %+v: want the link to be one of the two sessions", refs)
			}
			saved, other := refs[0].object, first
			if saved == first {
				other = second
			}
			if byEvent {
				p.expires(saved)
			} else if _, err := p.fake.Expire(saved); err != nil {
				t.Fatal(err)
			}

			p.act("op-replace-0002", "send", map[string]any{"id": record, "customer": "Second"})
			req2 := req
			req2.EffectID = p.entries()[1].id
			posts := len(p.posts())
			_, err := p.svc.OpenPaymentLink(ctx, req2)
			var pe *paylink.Error
			if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationLinkStillOpen {
				t.Fatalf("err = %v: want the refusal while the other session may be paid", err)
			}
			if len(p.posts()) != posts || len(p.fake.Sessions()) != 2 {
				t.Fatalf("a second link was made while session %s may be paid: %d sessions", other, len(p.fake.Sessions()))
			}
			if s, _ := p.fake.Session(other); s.Status != "open" {
				t.Fatalf("session %s = %s", other, s.Status)
			}
			if n := p.count(`SELECT count(*) FROM integration_refs`); n != 1 {
				t.Fatalf("%d references: a replacement was recorded", n)
			}
		})
	}
}

func TestAReplacementIsMadeOnceEveryObjectOfTheSavedLinksTokenEnded(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-replace-0011")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	first := p.fake.Sessions()[0].ID
	second := p.secondSessionOfToken(first)
	if _, err := p.svc.OpenPaymentLink(ctx, req); err != nil {
		t.Fatalf("the retry found no link: %v", err)
	}
	for _, id := range []string{first, second} {
		if _, err := p.fake.Expire(id); err != nil {
			t.Fatal(err)
		}
	}
	p.act("op-replace-0012", "send", map[string]any{"id": record, "customer": "Second"})
	req2 := req
	req2.EffectID = p.entries()[1].id
	link, err := p.svc.OpenPaymentLink(ctx, req2)
	if err != nil {
		t.Fatalf("no replacement although every session ended: %v", err)
	}
	if link.ObjectID == first || link.ObjectID == second || len(p.fake.Sessions()) != 3 {
		t.Fatalf("link = %+v, %d sessions", link, len(p.fake.Sessions()))
	}
}
