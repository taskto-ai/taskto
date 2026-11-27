package integrations

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/paylink"
)

func TestAKeyOfAnotherAccountBeforeTheWriteAheadStopsTheCallThatHoldsTheOldKey(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-account-ahead-0001")
	otherKey := "sk_" + "test_" + strings.Repeat("Othr", 6)
	p.svc.beforeBeginRef = func() {
		p.svc.beforeBeginRef = nil
		p.fake.SetAccount("acct_" + strings.Repeat("Othr", 4))
		p.fake.SetAPIKey(otherKey)
		if status, code := p.patchKey(otherKey); status != http.StatusOK {
			t.Errorf("the other account's key = %d %q", status, code)
		}
	}
	posts := len(p.posts())
	_, err := p.svc.OpenPaymentLink(ctx, req)
	var pe *paylink.Error
	if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationAccountChanged {
		t.Fatalf("err = %v: want the refusal of the call that holds the old account's key", err)
	}
	if len(p.posts()) != posts || len(p.fake.Sessions()) != 0 {
		t.Fatalf("a call went out for the old account: %d sessions", len(p.fake.Sessions()))
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE sends > clean_sends`); n != 0 {
		t.Fatalf("%d attempts written ahead", n)
	}
}

func TestAKeyOfTheSameAccountBeforeTheWriteAheadIsUsedForTheCall(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-account-ahead-0003")
	rotated := "sk_" + "test_" + strings.Repeat("Rota", 6)
	p.svc.beforeBeginRef = func() {
		p.svc.beforeBeginRef = nil
		p.fake.SetAPIKey(rotated)
		if status, code := p.patchKey(rotated); status != http.StatusOK {
			t.Errorf("a rotation of the key = %d %q", status, code)
		}
	}
	link, err := p.svc.OpenPaymentLink(ctx, req)
	if err != nil || link.ObjectID == "" {
		t.Fatalf("link = %+v, err = %v", link, err)
	}
	if n := len(p.fake.Sessions()); n != 1 {
		t.Fatalf("%d sessions after a rotation, want 1", n)
	}
}

func TestSquareAKeyOfAnotherMerchantBeforeTheWriteAheadStopsTheCallThatHoldsTheOldToken(t *testing.T) {
	ctx := context.Background()
	p := newSquarePay(t, publicURL)
	record := p.invoice(10, "USD")
	req := p.direct(record, "op-square-account-0001")
	req.Currency = "USD"
	other := "square-token-" + strings.Repeat("othr", 6)
	p.svc.beforeBeginRef = func() {
		p.svc.beforeBeginRef = nil
		p.sq.SetMerchant("MOTHER" + strings.Repeat("M", 7))
		p.sq.SetToken(other)
		if status, code := p.patchKey(other); status != http.StatusOK {
			t.Errorf("the other merchant's token = %d %q", status, code)
		}
	}
	_, err := p.svc.OpenPaymentLink(ctx, req)
	var pe *paylink.Error
	if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationAccountChanged {
		t.Fatalf("err = %v: want the refusal of the call that holds the old merchant's token", err)
	}
	if n := len(p.sq.Creates()); n != 0 || len(p.sq.Links()) != 0 {
		t.Fatalf("%d creations for the old merchant", n)
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE sends > clean_sends`); n != 0 {
		t.Fatalf("%d attempts written ahead", n)
	}
}

func TestSquareATokenOfTheSameMerchantBeforeTheWriteAheadIsUsedForTheCall(t *testing.T) {
	ctx := context.Background()
	p := newSquarePay(t, publicURL)
	record := p.invoice(10, "USD")
	req := p.direct(record, "op-square-account-0002")
	req.Currency = "USD"
	rotated := "square-token-" + strings.Repeat("rota", 6)
	p.svc.beforeBeginRef = func() {
		p.svc.beforeBeginRef = nil
		p.sq.SetToken(rotated)
		if status, code := p.patchKey(rotated); status != http.StatusOK {
			t.Errorf("a rotation of the token = %d %q", status, code)
		}
	}
	link, err := p.svc.OpenPaymentLink(ctx, req)
	if err != nil || link.ObjectID == "" {
		t.Fatalf("link = %+v, err = %v", link, err)
	}
	if n := len(p.sq.Links()); n != 1 {
		t.Fatalf("%d links after a rotation, want 1", n)
	}
}
