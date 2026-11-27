package integrations

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/integrations/testprovider"
	"taskto.ai/taskto/internal/paylink"
)

func TestAnOldKeyNeverInstallsItsObjectAfterTheRefMovedOn(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-stale-0001")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	old := p.fake.Sessions()[0]
	r, _, err := p.svc.refOf(ctx, req.EffectID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := p.svc.attemptByKey(ctx, r.id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.svc.markAttempt(ctx, a, attemptExpired); err != nil {
		t.Fatal(err)
	}
	if moved, err := p.svc.nextAttempt(ctx, r, a); err != nil || moved.keySeq != 1 {
		t.Fatalf("moved = %+v, %v", moved, err)
	}

	_, err = p.svc.noteLink(ctx, r, a, req, Link{ObjectID: old.ID, URL: *old.URL})
	var pe *paylink.Error
	if !errors.As(err, &pe) || !pe.Transient {
		t.Fatalf("err = %v: want a transient failure, the entry tries again", err)
	}
	if refs := p.refs(); len(refs) != 1 || refs[0].object != "" || refs[0].keySeq != 1 {
		t.Fatalf("refs = %+v: the old key's object was installed", refs)
	}

	q := newPay(t, publicURL)
	req = q.direct(q.invoice(10, "EUR"), "op-stale-0002")
	q.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := q.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	made := q.fake.Sessions()[0]
	r, _, _ = q.svc.refOf(ctx, req.EffectID)
	a, _ = q.svc.attemptByKey(ctx, r.id, 0)
	q.exec(`UPDATE integration_refs SET state = 'failed' WHERE id = ?`, r.id)
	if link, err := q.svc.noteLink(ctx, r, a, req, Link{ObjectID: made.ID, URL: *made.URL}); err != nil || link.ObjectID != made.ID {
		t.Fatalf("link = %+v, %v", link, err)
	}
}
