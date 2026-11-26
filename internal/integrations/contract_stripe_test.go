package integrations

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/integrations/testprovider"
)

// The Stripe adapter against the adapter contract suite (contract_test.go), with the fake Stripe.

func TestStripeMeetsThePaymentsContract(t *testing.T) {
	runPaymentsContract(t, stripeFixture)
}

// stripeFixture is the fixture of the Stripe adapter: its real client (pointed at the fake by the test
// endpoint, which only a constructor option can do), the fake's key and signing secret, and the fake's
// payer, expiry and refund.
func stripeFixture(t *testing.T) *paymentsFixture {
	t.Helper()
	fake := testprovider.NewStripe(t)
	sign := func(body []byte, secret string, at time.Time) http.Header {
		return http.Header{testprovider.SignatureHeader: {testprovider.Sign(secret, at, body)}}
	}
	deliver := func(evs []testprovider.Event) []signedDelivery {
		out := make([]signedDelivery, len(evs))
		for i, e := range evs {
			body := e.Body()
			out[i] = signedDelivery{Header: sign(body, testprovider.TestWebhookSecret, fake.Clock().Now()), Body: body}
		}
		return out
	}
	one := func(ev testprovider.Event) signedDelivery { return deliver([]testprovider.Event{ev})[0] }
	payer := fake.Payer()

	return &paymentsFixture{
		Name:     "stripe",
		Provider: stripe{},
		Client:   NewClient(Lookup("stripe"), nil, egress.MustParse("127.0.0.1"), WithTestEndpoint(fake.Host()), WithoutConnectionReuse()),
		Creds:    Credentials{Provider: "stripe", Mode: ModeTest, APIKey: Secret(fake.APIKey()), WebhookSecret: Secret(testprovider.TestWebhookSecret)},
		Shapes: []linkShape{
			// The server has a public address: a Checkout Session that returns the payer to its pages.
			{Name: "checkout session", Pages: true, PaymentCarriesToken: true, CanExpire: true},
			// It hasn't: a Payment Link with Stripe's own confirmation, which takes one payment and doesn't expire.
			{Name: "payment link", Pages: false},
		},
		Now:         fake.Clock().Now,
		Tolerance:   StripeTolerance,
		OtherSecret: testprovider.OtherWebhookSecret,
		PayerHosts:  []string{stripeCheckoutHost, stripeLinkHost},
		// The fake answers a key it doesn't know as Stripe does.
		RefusalWords: []string{"Invalid API Key"},
		PayerWords:   []string{payer.Email, payer.Name},
		Objects:      func() int { return len(fake.Sessions()) + len(fake.Links()) },
		Pay: func(l Link, sh linkShape) ([]signedDelivery, error) {
			var evs []testprovider.Event
			var err error
			if sh.Pages {
				evs, err = fake.Pay(l.ObjectID)
			} else {
				evs, err = fake.PayLink(l.ObjectID)
			}
			return deliver(evs), err
		},
		Expire: func(l Link, _ linkShape) ([]signedDelivery, error) {
			evs, err := fake.Expire(l.ObjectID)
			return deliver(evs), err
		},
		Refund: func(l Link, _ linkShape, minor int64) ([]signedDelivery, error) {
			evs, err := fake.Refund(l.ObjectID, minor)
			return deliver(evs), err
		},
		Unacted: func() signedDelivery {
			return one(testprovider.NewEvent("customer.created", map[string]any{"id": "cus_" + strings.Repeat("a", 14), "object": "customer"}, fake.Clock().Now()))
		},
		Foreign: func() signedDelivery {
			return one(testprovider.NewEvent(testprovider.EventSessionCompleted, map[string]any{
				"id": "cs_test_" + strings.Repeat("a", 40), "object": "checkout.session", "mode": "payment", "payment_status": "paid",
				"client_reference_id": nil, "amount_total": 1000, "currency": "eur", "payment_intent": "pi_" + strings.Repeat("a", 20),
			}, fake.Clock().Now()))
		},
		Sign: sign,
	}
}

// ----- the suite has teeth -----

// brokenStripe is the Stripe adapter with one fault, each of which a contract case must catch.
type brokenStripe struct {
	stripe
	// freshKeys asks for each call under a new token, so a retry makes a second link.
	freshKeys bool
	// acceptsAnything verifies every delivery.
	acceptsAnything bool
	// ignoresTime verifies a delivery whatever its signature's time.
	ignoresTime bool
	// leaksPayer puts what the payer typed into the event.
	leaksPayer string
}

func (b brokenStripe) CreatePaymentLink(ctx context.Context, creds Credentials, c *Client, l NewLink) (Link, error) {
	if b.freshKeys {
		l.Token, _ = newToken()
	}
	return b.stripe.CreatePaymentLink(ctx, creds, c, l)
}

var signedAt = regexp.MustCompile(`t=(\d+)`)

func (b brokenStripe) VerifyWebhook(h http.Header, body []byte, s WebhookSecrets, now time.Time) error {
	if b.acceptsAnything {
		return nil
	}
	if b.ignoresTime {
		if m := signedAt.FindStringSubmatch(h.Get(StripeSignatureHeader)); m != nil {
			if secs, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				now = time.Unix(secs, 0)
			}
		}
	}
	return b.stripe.VerifyWebhook(h, body, s, now)
}

func (b brokenStripe) Normalize(body []byte) (Event, bool) {
	ev, ok := b.stripe.Normalize(body)
	if ok && b.leaksPayer != "" {
		ev.PaymentID = b.leaksPayer
	}
	return ev, ok
}

func caseNamed(t *testing.T, prefix string) contractCase {
	t.Helper()
	for _, c := range paymentsContractCases {
		if strings.HasPrefix(c.name, prefix) {
			return c
		}
	}
	t.Fatalf("no contract case starts with %q", prefix)
	return contractCase{}
}

// An adapter that breaks what the server relies on fails the suite: the cases are worth having only if
// a fault they are about makes them fail.
func TestTheContractSuiteNoticesABrokenAdapter(t *testing.T) {
	payer := testprovider.DefaultPayer()
	for name, tc := range map[string]struct {
		fault brokenStripe
		// failing is the prefix of the case that must fail.
		failing string
	}{
		"a call under a new key each time":        {brokenStripe{freshKeys: true}, "a link is made once"},
		"every delivery verified":                 {brokenStripe{acceptsAnything: true}, "a delivery signed with another secret"},
		"a signature's time not checked":          {brokenStripe{ignoresTime: true}, "a delivery outside the time tolerance"},
		"a payer's email in the event":            {brokenStripe{leaksPayer: payer.Email}, "a payment is one succeeded event"},
		"a payer's name in the event":             {brokenStripe{leaksPayer: payer.Name}, "a payment is one succeeded event"},
		"a signature of the wrong secret passing": {brokenStripe{acceptsAnything: true}, "the previous secret verifies"},
	} {
		t.Run(name, func(t *testing.T) {
			fx := stripeFixture(t)
			fx.Provider = tc.fault
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := caseNamed(t, tc.failing).run(ctx, fx); err == nil {
				t.Fatalf("the case %q passes an adapter with the fault %q", tc.failing, name)
			}
		})
	}

	// The fixture alone is sound: the same cases pass the real adapter (TestStripeMeetsThePaymentsContract),
	// and the payer's words are in what the provider sends, so the check for them can fail.
	fx := stripeFixture(t)
	l, _, err := fx.made(context.Background(), fx.Shapes[0])
	if err != nil {
		t.Fatal(err)
	}
	ds, err := fx.Pay(l, fx.Shapes[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range fx.PayerWords {
		found := false
		for _, d := range ds {
			found = found || strings.Contains(string(d.Body), w)
		}
		if !found {
			t.Errorf("the payer's %q isn't in what the provider sends for a payment", w)
		}
	}
}
