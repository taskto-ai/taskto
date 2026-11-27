package integrations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/integrations/testprovider"
	"taskto.ai/taskto/internal/paylink"
)

// rawStripe is a client of a server that answers every request with body.
func rawStripe(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(srv.Close)
	return NewClient(Lookup("stripe"), nil, egress.MustParse("127.0.0.1"), WithTestEndpoint(strings.TrimPrefix(srv.URL, "http://")))
}

// A Checkout Session is ours only in the mode our calls make (payment): one of the token in another mode
// is never adopted, and its expiry is never taken for the end of the key's call, whether a lookup lists
// it or its state is read. An expired one of the token is named, so that the try can tell it is the
// object the key has on record.
func TestASessionsExpiryIsTakenOnlyFromOneOfOurMode(t *testing.T) {
	token := strings.Repeat("a", 32)
	id := "cs_test_" + strings.Repeat("b", 24)
	nl := NewLink{Token: token, AmountMinor: 100, Currency: "EUR", SuccessURL: publicURL + PaymentDonePath}
	session := func(status, mode string) string {
		return fmt.Sprintf(`{"id":%q,"object":"checkout.session","url":"https://checkout.stripe.com/c/pay/%s","currency":"eur","status":%q,"mode":%q,"amount_total":100,"client_reference_id":%q}`,
			id, id, status, mode, token)
	}
	list := func(s string) string { return `{"object":"list","has_more":false,"data":[` + s + `]}` }
	creds := Credentials{APIKey: Secret("not-a-key")}

	for _, status := range []string{"open", "expired"} {
		t.Run("a "+status+" session in another mode", func(t *testing.T) {
			seen, found, expired, err := stripe{}.FindPaymentLink(context.Background(), creds, rawStripe(t, list(session(status, "subscription"))), nl, time.Now())
			if ce, ok := IsCall(err); !ok || ce.Kind != CallRefused || found || expired || seen.ObjectID != id {
				t.Fatalf("seen %+v, found %v, expired %v, err %v: want it refused and named", seen, found, expired, err)
			}
			if _, err := (stripe{}).LinkState(context.Background(), creds, rawStripe(t, session(status, "setup")), false, id); err == nil {
				t.Fatalf("the state of a session in another mode was read as ours")
			}
		})
	}
	t.Run("an expired session of ours", func(t *testing.T) {
		seen, found, expired, err := stripe{}.FindPaymentLink(context.Background(), creds, rawStripe(t, list(session("expired", "payment"))), nl, time.Now())
		if err != nil || found || !expired || seen.ObjectID != id {
			t.Fatalf("seen %+v, found %v, expired %v, err %v: want it expired and named", seen, found, expired, err)
		}
		if st, err := (stripe{}).LinkState(context.Background(), creds, rawStripe(t, session("expired", "payment")), false, id); err != nil || st != LinkExpired {
			t.Fatalf("state = %v, %v", st, err)
		}
	})
}

// A key that has an object on record moves on only if that very object is seen expired: an expired
// session of the key's token with another id says nothing of the one on record, which may be open. The
// entry waits, and nothing is made under a new key.
func TestAKeyMovesOnOnlyWhenItsOwnObjectIsSeenExpired(t *testing.T) {
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-identity-0001")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(context.Background(), req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	if _, err := p.fake.Expire(p.fake.Sessions()[0].ID); err != nil {
		t.Fatal(err)
	}
	// Another object of the key is on record, seen and not checked.
	p.exec(`UPDATE integration_ref_attempts SET object_id = ?, object_verified = 0 WHERE seq = 0`, "cs_test_"+strings.Repeat("c", 24))
	_, err := p.svc.OpenPaymentLink(context.Background(), req)
	var pe *paylink.Error
	if !errors.As(err, &pe) || pe.Code != apierr.CodeIntegrationLinkLookupPending {
		t.Fatalf("err = %v: want the wait", err)
	}
	if posts := p.posts(); len(posts) != 1 {
		t.Fatalf("%d calls: a new key was taken while the object on record may be open", len(posts))
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE outcome = 'expired'`); n != 0 {
		t.Fatal("the key was taken as expired")
	}
}
