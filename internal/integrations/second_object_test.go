package integrations

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

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

// Another try notes an object A on the key while this try's lookup runs from an older read of the key
// (which had none). What this try finds (a session B that expired, or one it would install) never takes
// the key from A: the key isn't marked expired, and B is never installed as the ref's link.
func TestAnOlderReadOfTheKeyNeverOverridesTheObjectNotedSince(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*pay, paylink.Request, ref, attempt, string) {
		p := newPay(t, publicURL)
		req := p.direct(p.invoice(10, "EUR"), "op-stale-read-0001")
		p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
		if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
			t.Fatal("the lost answer was no error")
		}
		r, _, err := p.svc.refOf(ctx, req.EffectID)
		if err != nil {
			t.Fatal(err)
		}
		a, err := p.svc.attemptByKey(ctx, r.id, 0)
		if err != nil {
			t.Fatal(err)
		}
		other := "cs_test_" + strings.Repeat("d", 24)
		p.exec(`UPDATE integration_ref_attempts SET object_id = ?, object_verified = 0 WHERE seq = 0`, other)
		return p, req, r, a, other
	}
	t.Run("an expired one found by the lookup", func(t *testing.T) {
		p, req, r, a, other := setup(t)
		if _, err := p.fake.Expire(p.fake.Sessions()[0].ID); err != nil {
			t.Fatal(err)
		}
		rec, _ := p.svc.store.getSnapshot(ctx, p.ws, p.in.ID)
		creds, _ := p.svc.store.credentials(ctx, p.ws, p.in.ID)
		tr := try{s: p.svc, provider: Lookup(rec.Provider).(PaymentProvider), creds: creds,
			client: p.svc.Client(Lookup(rec.Provider), rec.Settings, p.svc.clientOpts...), rec: rec, req: req, since: time.Now().Add(-time.Hour)}
		res, _, err := tr.resolve(ctx, &r, a)
		var pe *paylink.Error
		if res == resolvedExpired || !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationLinkLookupPending {
			t.Fatalf("res = %v, err = %v: want the wait", res, err)
		}
		if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE seq = 0 AND object_id = ? AND outcome IS NULL`, other); n != 1 {
			t.Fatal("the key was marked expired while another object is on record")
		}
	})
	t.Run("one installed as the link", func(t *testing.T) {
		p, req, r, a, other := setup(t)
		s := p.fake.Sessions()[0]
		_, err := p.svc.noteLink(ctx, r, a, req, Link{ObjectID: s.ID, URL: *s.URL})
		var pe *paylink.Error
		if !errors.As(err, &pe) || !pe.Transient {
			t.Fatalf("err = %v: want a transient failure", err)
		}
		if refs := p.refs(); refs[0].object != "" {
			t.Fatalf("refs = %+v: installed over the object on record", refs)
		}
		if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE seq = 0 AND object_id = ? AND outcome IS NULL`, other); n != 1 {
			t.Fatal("the object on record was replaced")
		}
	})
}

// The first call's answer was lost (no object on record, session A open at Stripe), and the lookup
// lists another session B of the key's token first, which expires before its state is read. B's expiry
// doesn't move the ref on while A, another object of the token, may still be paid: the lookup is read
// again, and the entry waits for a person instead of taking a new key.
func TestAnExpiryMovesTheRefOnOnlyWhenNoOtherObjectOfTheTokenMayBePaid(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	req := p.direct(p.invoice(10, "EUR"), "op-survivor-0001")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	a := p.fake.Sessions()[0]
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
			p.svc.beforeSettle = nil
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
		t.Fatal("a call went out while session A of the key's token may be paid")
	}
	if r := p.refs(); r[0].keySeq != 0 {
		t.Fatalf("refs = %+v: the ref moved on", r)
	}
	if s, _ := p.fake.Session(a.ID); s.Status != "open" {
		t.Fatalf("A = %s", s.Status)
	}
}
