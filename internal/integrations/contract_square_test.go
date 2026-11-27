package integrations

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/integrations/testprovider"
)

// The Square adapter against the adapter contract suite (contract_test.go), with the fake Square.

func TestSquareMeetsThePaymentsContract(t *testing.T) {
	runPaymentsContract(t, squareFixture)
}

// squareSettings are a sandbox integration's at the fake's location.
func squareSettings() map[string]string {
	return map[string]string{"environment": "sandbox", "location_id": testprovider.SquareLocationID}
}

// squareHookURL is the webhook address the fixture's deliveries are signed over. Its token is built
// from pieces, never written as one hex literal.
var squareHookURL = "https://taskto.example.test/hooks/" + strings.Repeat("0123456789abcdef", 2)

// squareFixture is the fixture of the Square adapter: its real client pointed at the fake, the fake's
// token and signature key, and the fake's payer and refund. A Square link doesn't expire.
func squareFixture(t *testing.T) *paymentsFixture {
	t.Helper()
	clock := testprovider.NewManualClock(time.Time{})
	fake := testprovider.NewSquare(t, testprovider.WithSquareClock(clock))
	// Square signs its time inside the body (created_at), over the address and the body.
	sign := func(body []byte, secret string, at time.Time) signedDelivery {
		body = testprovider.RestampSquare(body, at)
		return signedDelivery{Header: http.Header{SquareSignatureHeader: {testprovider.SignSquare(secret, squareHookURL, body)}}, Body: body}
	}
	deliver := func(evs []testprovider.SquareEvent, err error) ([]signedDelivery, error) {
		out := make([]signedDelivery, len(evs))
		for i, e := range evs {
			body := e.Body()
			out[i] = signedDelivery{Header: http.Header{SquareSignatureHeader: {testprovider.SignSquare(testprovider.SquareSignatureKey, squareHookURL, body)}}, Body: body}
		}
		return out, err
	}
	one := func(ev testprovider.SquareEvent) signedDelivery {
		ds, _ := deliver([]testprovider.SquareEvent{ev}, nil)
		return ds[0]
	}
	payer := fake.Payer()
	settings := squareSettings()

	return &paymentsFixture{
		Name:     "square",
		Provider: square{},
		Client:   NewClient(Lookup("square"), settings, egress.MustParse("127.0.0.1"), WithTestEndpoint(fake.Host()), WithoutConnectionReuse()),
		Creds: Credentials{
			Provider: "square", Mode: ModeTest, Settings: settings,
			APIKey: Secret(testprovider.SquareTestToken), WebhookSecret: Secret(testprovider.SquareSignatureKey),
		},
		Shapes: []linkShape{
			// The server has a public address: the payer is sent back to its done page.
			{Name: "redirect to the server", Pages: true, PaymentCarriesToken: true},
			// It hasn't: the payer stays on Square's confirmation.
			{Name: "Square's confirmation", Pages: false, PaymentCarriesToken: true},
		},
		Now:         clock.Now,
		Tolerance:   squareRetryWindow + squareSkew,
		Ahead:       squareSkew,
		URL:         squareHookURL,
		OtherSecret: testprovider.SquareOtherSignatureKey,
		// The fake's links are on Square's sandbox link host, as a sandbox integration's are.
		PayerHosts: []string{squareLinkHost(settings)},
		// The fake's refusal of a token says these: none may reach an error.
		RefusalWords: []string{"an error of the fake Square", "UNAUTHORIZED"},
		PayerWords:   []string{payer.Email, payer.Name},
		Objects: func() int {
			n := 0
			for _, l := range fake.Links() {
				if !l.Deleted {
					n++
				}
			}
			return n
		},
		Pay: func(l Link, _ linkShape) ([]signedDelivery, error) {
			return deliver(fake.PaySquare(l.ObjectID))
		},
		Refund: func(l Link, _ linkShape, minor int64) ([]signedDelivery, error) {
			return deliver(fake.RefundSquare(l.ObjectID, minor))
		},
		Unacted: func() signedDelivery {
			return one(testprovider.NewSquareEvent("customer.created", "customer", strings.Repeat("C", 26),
				map[string]any{"customer": map[string]any{"id": strings.Repeat("C", 26)}}, clock.Now()))
		},
		Foreign: func() signedDelivery {
			id := strings.Repeat("P", 29)
			return one(testprovider.NewSquareEvent("payment.updated", "payment", id, map[string]any{"payment": map[string]any{
				"id": id, "status": "COMPLETED", "total_money": map[string]any{"amount": 1000, "currency": "EUR"},
				"amount_money": map[string]any{"amount": 1000, "currency": "EUR"}, "order_id": strings.Repeat("O", 29),
			}}, clock.Now()))
		},
		Sign: sign,
	}
}
