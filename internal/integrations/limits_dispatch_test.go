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

// The ceiling is lowered (or the integration switched off) after a resend under an unknown key was
// written ahead and before it leaves: the call is not sent, and is noted as one that never ran, so the
// key's outcome is as it was and nothing is made past the new limits.
func TestALimitChangedBeforeTheCallLeavesStopsIt(t *testing.T) {
	for name, change := range map[string]string{
		"the ceiling lowered":          `UPDATE integrations SET max_amount_minor = 500 WHERE id = ?`,
		"the integration switched off": `UPDATE integrations SET disabled_at = CURRENT_TIMESTAMP WHERE id = ?`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			p := newPay(t, publicURL)
			req := p.direct(p.invoice(10, "EUR"), "op-dispatch-0001")
			p.fake.Fail(testprovider.ConnectionReset().On(http.MethodPost, "/v1/checkout/sessions"))
			if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
				t.Fatal("the dropped call was no error")
			}
			p.svc.afterWriteAhead = func() error {
				p.svc.afterWriteAhead = nil
				p.exec(change, p.in.ID)
				return nil
			}
			_, err := p.svc.OpenPaymentLink(ctx, req)
			var pe *paylink.Error
			if !errors.As(err, &pe) || (pe.Code != apierr.CodeIntegrationLinkLookupPending && pe.Code != apierr.CodeIntegrationUnavailable) {
				t.Fatalf("err = %v", err)
			}
			if n := len(p.fake.RequestsTo(http.MethodPost, "/v1/checkout/sessions")); n != 1 {
				t.Fatalf("%d calls: the resend left past the change", n)
			}
			if n := p.objects("/v1/checkout/sessions"); n != 0 {
				t.Fatalf("%d sessions made", n)
			}
			if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE sends = 2 AND clean_sends = 1`); n != 1 {
				t.Fatal("the stopped call was not noted as one that never ran")
			}
		})
	}
}

// The final check before the call leaves fails for a reason that isn't a refusal (the try's context
// ended, the settings couldn't be read): the call never left, so it is noted as one that never ran,
// and the key's outcome stays known. The next try asks the same key, with no lookup first.
func TestAFailedFinalCheckLeavesTheCallNotExecuted(t *testing.T) {
	p := newPay(t, publicURL)
	req := p.direct(p.invoice(10, "EUR"), "op-final-0001")
	ctx, cancel := context.WithCancel(context.Background())
	p.svc.afterWriteAhead = func() error {
		p.svc.afterWriteAhead = nil
		cancel()
		return nil
	}
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the ended try was no error")
	}
	if n := len(p.fake.RequestsTo(http.MethodPost, "/v1/checkout/sessions")); n != 0 {
		t.Fatalf("%d calls left", n)
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE sends = 1 AND clean_sends = 1 AND unknown_since IS NULL`); n != 1 {
		t.Fatal("the call that never left was left with an unknown outcome")
	}
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if n := len(p.fake.RequestsTo(http.MethodGet, "/v1/checkout/sessions")); n != 0 {
		t.Fatalf("%d lookups: the next try took the key's outcome for unknown", n)
	}
}
