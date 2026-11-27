package integrations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/paylink"
)

func (p *pay) patchKey(key string) (int, string) {
	p.t.Helper()
	rec := p.owner.do(http.MethodPatch, wsPath(p.ws, fmt.Sprintf("integrations/%d", p.in.ID)), map[string]any{"secrets": map[string]string{"api_key": key}})
	body := decode[struct {
		Code string `json:"code"`
	}](p.t, rec)
	return rec.Code, body.Code
}

func TestAKeyOfTheOtherModeBeforeTheWriteAheadStopsTheCallThatHoldsTheOldKey(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	p.setKey(stripeLiveKey)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-mode-ahead-0001")
	p.svc.beforeBeginRef = func() {
		p.svc.beforeBeginRef = nil
		if status, code := p.patchKey(stripeTestKey); status != http.StatusOK {
			t.Errorf("the switch with no payable link = %d %q", status, code)
		}
	}
	posts := len(p.posts())
	_, err := p.svc.OpenPaymentLink(ctx, req)
	var pe *paylink.Error
	if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationAccountChanged {
		t.Fatalf("err = %v: want the refusal of the call that holds the old key", err)
	}
	if len(p.posts()) != posts || len(p.fake.Sessions()) != 0 {
		t.Fatalf("a call went out with the key of the old mode: %d sessions", len(p.fake.Sessions()))
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE sends > clean_sends`); n != 0 {
		t.Fatalf("%d attempts written ahead", n)
	}
	var mode string
	if err := p.db.QueryRow(`SELECT mode FROM integrations WHERE id = ?`, p.in.ID).Scan(&mode); err != nil || mode != "test" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
}

func TestAKeyOfTheOtherModeAfterTheWriteAheadIsRefusedAndTheCallGoesOn(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	p.setKey(stripeLiveKey)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-mode-ahead-0002")
	var status int
	var code string
	p.svc.afterWriteAhead = func() error {
		p.svc.afterWriteAhead = nil
		status, code = p.patchKey(stripeTestKey)
		return nil
	}
	link, err := p.svc.OpenPaymentLink(ctx, req)
	if err != nil || link.ObjectID == "" {
		t.Fatalf("link = %+v, err = %v", link, err)
	}
	if status != http.StatusConflict || code != string(apierr.CodeIntegrationModeChangeRefused) {
		t.Fatalf("the switch after the write-ahead = %d %q", status, code)
	}
	var mode string
	if err := p.db.QueryRow(`SELECT mode FROM integrations WHERE id = ?`, p.in.ID).Scan(&mode); err != nil || mode != ModeLive {
		t.Fatalf("mode = %q, %v", mode, err)
	}
}

func TestAKeyOfTheSameModeBeforeTheWriteAheadLetsTheCallGoOn(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	p.setKey(stripeLiveKey)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-mode-ahead-0003")
	rotated := "sk_" + "live_" + strings.Repeat("Rota", 6)
	p.svc.beforeBeginRef = func() {
		p.svc.beforeBeginRef = nil
		p.fake.SetAPIKey(rotated)
		if status, code := p.patchKey(rotated); status != http.StatusOK {
			t.Errorf("a rotation of the live key = %d %q", status, code)
		}
	}
	link, err := p.svc.OpenPaymentLink(ctx, req)
	if err != nil || link.ObjectID == "" {
		t.Fatalf("link = %+v, err = %v", link, err)
	}
}
