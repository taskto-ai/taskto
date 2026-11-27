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

func TestStripeMeetsThePaymentsContract(t *testing.T) {
	runPaymentsContract(t, stripeFixture)
}

func stripeFixture(t *testing.T) *paymentsFixture {
	t.Helper()
	clock := testprovider.NewManualClock(time.Now().UTC())
	fake := testprovider.NewStripe(t, testprovider.WithClock(clock))
	sign := func(body []byte, secret string, at time.Time) signedDelivery {
		return signedDelivery{Header: http.Header{testprovider.SignatureHeader: {testprovider.Sign(secret, at, body)}}, Body: body}
	}
	deliver := func(evs []testprovider.Event) []signedDelivery {
		out := make([]signedDelivery, len(evs))
		for i, e := range evs {
			out[i] = sign(e.Body(), testprovider.TestWebhookSecret, fake.Clock().Now())
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
			{Name: "checkout session", Pages: true, PaymentCarriesToken: true, CanExpire: true},
			{Name: "payment link", Pages: false},
		},
		Now:          fake.Clock().Now,
		Later:        clock.Advance,
		Tolerance:    StripeTolerance,
		OtherSecret:  testprovider.OtherWebhookSecret,
		PayerHosts:   []string{stripeCheckoutHost, stripeLinkHost},
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

type brokenStripe struct {
	stripe
	freshKeys            bool
	acceptsAnything      bool
	ignoresTime          bool
	leaksPayer           string
	datesByRead, undated bool
	now                  func() time.Time
}

func (b brokenStripe) ReadPayment(ctx context.Context, creds Credentials, c *Client, hosted bool, objectID string) (Observed, error) {
	obs, err := b.stripe.ReadPayment(ctx, creds, c, hosted, objectID)
	for i := range obs.Payments {
		switch {
		case b.datesByRead:
			obs.Payments[i].PaidAt = b.now().Truncate(time.Second)
		case b.undated:
			obs.Payments[i].PaidAt = time.Time{}
		}
	}
	return obs, err
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

func TestTheContractSuiteNoticesABrokenAdapter(t *testing.T) {
	payer := testprovider.DefaultPayer()
	for name, tc := range map[string]struct {
		fault   brokenStripe
		failing string
	}{
		"a call under a new key each time":        {brokenStripe{freshKeys: true}, "a link is made once"},
		"every delivery verified":                 {brokenStripe{acceptsAnything: true}, "a delivery signed with another secret"},
		"a signature's time not checked":          {brokenStripe{ignoresTime: true}, "a delivery outside the time tolerance"},
		"a payer's email in the event":            {brokenStripe{leaksPayer: payer.Email}, "a payment is one succeeded event"},
		"a payer's name in the event":             {brokenStripe{leaksPayer: payer.Name}, "a payment is one succeeded event"},
		"a signature of the wrong secret passing": {brokenStripe{acceptsAnything: true}, "the previous secret verifies"},
		"a payment dated by its read":             {brokenStripe{datesByRead: true}, "a payment read back is dated"},
		"a payment read with no time":             {brokenStripe{undated: true}, "a payment read back is dated"},
	} {
		t.Run(name, func(t *testing.T) {
			fx := stripeFixture(t)
			fault := tc.fault
			fault.now = fx.Now
			fx.Provider = fault
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := caseNamed(t, tc.failing).run(ctx, fx); err == nil {
				t.Fatalf("the case %q passes an adapter with the fault %q", tc.failing, name)
			}
		})
	}

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
