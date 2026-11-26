package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The adapter contract suite: what every payments adapter must do, written once, in terms of the
// adapter's own interfaces (PaymentProvider and Receiver) and the normalized shapes (Link, LinkState,
// *CallError, Event), so the behaviour the rest of the server relies on is pinned in one place and not
// once per provider.
//
// A provider's adapter runs the suite by giving a fixture: its adapter, a client that reaches its fake,
// credentials, and the few things only the fake can do (have a payer pay, expire or refund a link and
// hand over the provider's signed deliveries for it). The Stripe fixture is contract_stripe_test.go.
// To run the suite against another adapter:
//
//	func TestSquareMeetsThePaymentsContract(t *testing.T) { runPaymentsContract(t, squareFixture) }
//
// where squareFixture(t) builds a *paymentsFixture of the Square adapter and its fake. No case reads a
// provider's own words or ids: a case that needs to would be one that belongs to the adapter's own
// tests (Stripe's are paymentlink_test.go and stripe_webhook_test.go).
//
// Each case returns an error rather than failing a test, so the suite can also be run against an adapter
// that is broken on purpose, to show that it notices (TestTheContractSuiteNoticesABrokenAdapter).

// contractProvider is an adapter that makes payment links and receives webhooks.
type contractProvider interface {
	PaymentProvider
	Receiver
}

// linkShape is one of the shapes of link an adapter makes.
type linkShape struct {
	Name string
	// Pages is whether the link is asked with the server's own pages (a public address); false is the
	// provider's own confirmation page.
	Pages bool
	// PaymentCarriesToken says that a payment through this shape's link reaches the webhook with the
	// ref's token, and is mapped to a ref by it.
	PaymentCarriesToken bool
	// CanExpire says that a link of this shape can expire before it is paid.
	CanExpire bool
}

// signedDelivery is a webhook delivery as the provider sends it: its headers (with the signature) and
// the exact body.
type signedDelivery struct {
	Header http.Header
	Body   []byte
}

// paymentsFixture is what an adapter needs to run the suite.
type paymentsFixture struct {
	Name     string
	Provider contractProvider
	// Client reaches the adapter's fake, as the server's client reaches the provider.
	Client *Client
	// Creds are the integration's: its API key, and the webhook secret the fake signs with.
	Creds  Credentials
	Shapes []linkShape
	// Now is the clock the fake signs and stamps with, and the one verification is told.
	Now func() time.Time
	// Tolerance is how far a signature's time may be from now, in either direction.
	Tolerance time.Duration
	// OtherSecret is a second webhook secret of the provider's format that isn't the integration's.
	OtherSecret string
	// PayerWords are what a payer typed at the provider and the provider sends in its events: none of it
	// may come out of Normalize.
	PayerWords []string
	// Objects is how many payable objects the fake holds.
	Objects func() int
	// Pay has a payer pay the link, and returns every delivery the provider sends for the payment, in the
	// order it sends them. Expire and Refund (the latter of an amount in minor units; the rest of the
	// payment when 0) are optional: nil skips what needs them.
	Pay    func(l Link, sh linkShape) ([]signedDelivery, error)
	Expire func(l Link, sh linkShape) ([]signedDelivery, error)
	Refund func(l Link, sh linkShape, minor int64) ([]signedDelivery, error)
	// Unacted is a validly signed delivery of a type the adapter doesn't act on. Foreign is a validly
	// signed event of a payment that none of the server's links made (it carries no reference of ours).
	Unacted func() signedDelivery
	Foreign func() signedDelivery
	// Sign signs a body as the provider does, with the secret, at the time.
	Sign func(body []byte, secret string, at time.Time) http.Header
}

// contractCase is one thing every adapter must do.
type contractCase struct {
	name string
	run  func(ctx context.Context, fx *paymentsFixture) error
}

// runPaymentsContract runs every case against the fixture newFixture makes (a new one for each).
func runPaymentsContract(t *testing.T, newFixture func(t *testing.T) *paymentsFixture) {
	t.Helper()
	for _, c := range paymentsContractCases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := c.run(ctx, fx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// failf is an error that reads as a failure of the case.
func failf(format string, args ...any) error { return fmt.Errorf(format, args...) }

const (
	contractMinor    = 12010
	contractCurrency = "EUR"
)

func (sh linkShape) newLink(token string) NewLink {
	l := NewLink{Token: token, AmountMinor: contractMinor, Currency: contractCurrency, Description: "Invoice A-1"}
	if sh.Pages {
		l.SuccessURL, l.CancelURL = publicURL+PaymentDonePath, publicURL+PaymentCancelledPath
	}
	return l
}

// made creates a link of the shape and returns it with the terms it was asked with.
func (fx *paymentsFixture) made(ctx context.Context, sh linkShape) (Link, NewLink, error) {
	tok, err := newToken()
	if err != nil {
		return Link{}, NewLink{}, err
	}
	nl := sh.newLink(tok)
	l, err := fx.Provider.CreatePaymentLink(ctx, fx.Creds, fx.Client, nl)
	if err != nil {
		return Link{}, nl, failf("%s: creating a link: %w", sh.Name, err)
	}
	return l, nl, nil
}

// carrying is the shapes whose payments reach the webhook with the ref's token.
func (fx *paymentsFixture) carrying() []linkShape {
	var out []linkShape
	for _, sh := range fx.Shapes {
		if sh.PaymentCarriesToken {
			out = append(out, sh)
		}
	}
	return out
}

func isCall(err error, kind CallKind) bool {
	ce, ok := IsCall(err)
	return ok && ce.Kind == kind
}

var paymentsContractCases = []contractCase{
	{"the adapter is a payments provider with constant hosts and a pinned version", contractIdentity},
	{"a link is made once for a token", contractCreateOnce},
	{"a token asked again with other terms is refused and makes nothing", contractCreateConflict},
	{"another token makes another link", contractCreateAnother},
	{"a key the provider doesn't accept is a credentials failure that quotes nothing", contractCreateCredentials},
	{"the account of a key is one stable id", contractAccount},
	{"a link is found by its token", contractFindByToken},
	{"a link is adopted only if it asks the terms asked now", contractFindTerms},
	{"the state of a link is read live", contractLinkState},
	{"a genuine delivery verifies", contractVerifyGenuine},
	{"a delivery signed with another secret, unsigned or tampered with doesn't verify", contractVerifyForged},
	{"a delivery outside the time tolerance doesn't verify", contractVerifyTolerance},
	{"the previous secret verifies during a rotation, and only then", contractVerifyRotation},
	{"a verification's error never holds a secret", contractVerifyQuotesNothing},
	{"a payment is one succeeded event with the ref's token, the exact amount and the payer's words nowhere", contractNormalizePaid},
	{"an expiry is one expired event with the ref's token", contractNormalizeExpired},
	{"a refund is one refunded event of what has been refunded so far, found through the payment", contractNormalizeRefund},
	{"what the adapter doesn't act on, and what isn't ours, is not an event", contractNormalizeIgnored},
	{"a body that isn't an event is not an event", contractNormalizeGarbage},
}

func contractIdentity(_ context.Context, fx *paymentsFixture) error {
	p := fx.Provider
	if !slices.Contains(p.Kinds(), KindPayments) {
		return failf("kinds = %v, want %q among them", p.Kinds(), KindPayments)
	}
	if p.APIVersion() == "" {
		return failf("the adapter pins no API version: every call must send one, and a change of it is a change of code")
	}
	hosts := p.Hosts(map[string]string{"environment": "sandbox"})
	if len(hosts) == 0 {
		return failf("the adapter owns no host")
	}
	for _, h := range hosts {
		if strings.ContainsAny(h, "/:@?#") || h != strings.ToLower(h) {
			return failf("host %q isn't a bare lower-case host name", h)
		}
	}
	// The client refuses any address that isn't the adapter's: no setting or definition can aim a call
	// (and the credentials in it) at another host.
	ctx := context.Background()
	for _, other := range []string{"https://example.invalid/v1/x", "https://" + hosts[0] + ".example.invalid/", "http://" + hosts[0] + "/", "https://user@" + hosts[0] + "/"} {
		if _, err := fx.Client.NewRequest(ctx, http.MethodGet, other, nil); !errors.Is(err, ErrHostNotOwned) {
			return failf("a request to %s wasn't refused: %v", other, err)
		}
	}
	for _, s := range p.Secrets() {
		if s.Name == SecretAPIKey && !s.Required {
			return failf("the API key must be a required secret")
		}
	}
	return nil
}

func contractCreateOnce(ctx context.Context, fx *paymentsFixture) error {
	for _, sh := range fx.Shapes {
		before := fx.Objects()
		first, nl, err := fx.made(ctx, sh)
		if err != nil {
			return err
		}
		if first.ObjectID == "" || first.URL == "" || len(first.ObjectID) > maxLinkID || len(first.URL) > maxLinkURL {
			return failf("%s: link = %+v", sh.Name, first)
		}
		u, err := url.Parse(first.URL)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
			return failf("%s: the address %q isn't a plain https address", sh.Name, first.URL)
		}
		// The same key and the same terms again (a retry after a lost answer) is the same link, not a second.
		for range 2 {
			again, err := fx.Provider.CreatePaymentLink(ctx, fx.Creds, fx.Client, nl)
			if err != nil {
				return failf("%s: asking again under the same key: %w", sh.Name, err)
			}
			if again.ObjectID != first.ObjectID || again.URL != first.URL {
				return failf("%s: asking again made %+v, first %+v", sh.Name, again, first)
			}
		}
		if n := fx.Objects() - before; n != 1 {
			return failf("%s: three calls under one key made %d objects", sh.Name, n)
		}
	}
	return nil
}

func contractCreateConflict(ctx context.Context, fx *paymentsFixture) error {
	for _, sh := range fx.Shapes {
		first, nl, err := fx.made(ctx, sh)
		if err != nil {
			return err
		}
		before := fx.Objects()
		other := nl
		other.AmountMinor++
		got, err := fx.Provider.CreatePaymentLink(ctx, fx.Creds, fx.Client, other)
		if _, ok := IsCall(err); !ok {
			return failf("%s: the same key with another amount: %+v, %v, want a *CallError", sh.Name, got, err)
		}
		if got != (Link{}) {
			return failf("%s: the same key with another amount gave a link: %+v", sh.Name, got)
		}
		if n := fx.Objects() - before; n != 0 {
			return failf("%s: the refused call made %d objects", sh.Name, n)
		}
		// What the key made is still what it made.
		again, err := fx.Provider.CreatePaymentLink(ctx, fx.Creds, fx.Client, nl)
		if err != nil || again.ObjectID != first.ObjectID {
			return failf("%s: after a refused call the key's link is %+v, %v, want %+v", sh.Name, again, err, first)
		}
	}
	return nil
}

func contractCreateAnother(ctx context.Context, fx *paymentsFixture) error {
	for _, sh := range fx.Shapes {
		a, _, err := fx.made(ctx, sh)
		if err != nil {
			return err
		}
		b, _, err := fx.made(ctx, sh)
		if err != nil {
			return err
		}
		if a.ObjectID == b.ObjectID || a.URL == b.URL {
			return failf("%s: two tokens made one link: %+v %+v", sh.Name, a, b)
		}
	}
	return nil
}

func contractCreateCredentials(ctx context.Context, fx *paymentsFixture) error {
	bad := fx.Creds
	bad.APIKey = Secret(strings.Repeat("x", 4) + "-not-the-key-" + strings.Repeat("y", 8))
	for _, sh := range fx.Shapes {
		tok, err := newToken()
		if err != nil {
			return err
		}
		before := fx.Objects()
		_, err = fx.Provider.CreatePaymentLink(ctx, bad, fx.Client, sh.newLink(tok))
		ce, ok := IsCall(err)
		if !ok || ce.Kind != CallCredentials {
			return failf("%s: a key the provider doesn't accept: %v, want a credentials failure", sh.Name, err)
		}
		if strings.Contains(err.Error(), bad.APIKey.Reveal()) || strings.Contains(err.Error(), fx.Creds.APIKey.Reveal()) {
			return failf("%s: the failure quotes a key: %v", sh.Name, err)
		}
		if fx.Objects() != before {
			return failf("%s: a refused key made an object", sh.Name)
		}
		if _, _, _, err := fx.Provider.FindPaymentLink(ctx, bad, fx.Client, sh.newLink(tok), fx.Now().Add(-time.Hour)); !isCall(err, CallCredentials) {
			return failf("%s: a lookup with a key the provider doesn't accept: %v", sh.Name, err)
		}
		if _, err := fx.Provider.LinkState(ctx, bad, fx.Client, !sh.Pages, "unknown"); err == nil {
			return failf("%s: a state read with a key the provider doesn't accept succeeded", sh.Name)
		}
	}
	return nil
}

func contractAccount(ctx context.Context, fx *paymentsFixture) error {
	a, err := fx.Provider.AccountID(ctx, fx.Creds, fx.Client)
	if err != nil || a == "" || len(a) > 255 || strings.ContainsAny(a, " \t\r\n") {
		return failf("account = %q, %v", a, err)
	}
	if b, err := fx.Provider.AccountID(ctx, fx.Creds, fx.Client); err != nil || b != a {
		return failf("the account changed between two reads: %q then %q, %v", a, b, err)
	}
	bad := fx.Creds
	bad.APIKey = Secret("not-the-key-" + strings.Repeat("y", 12))
	if _, err := fx.Provider.AccountID(ctx, bad, fx.Client); !isCall(err, CallCredentials) {
		return failf("the account of a key the provider doesn't accept: %v", err)
	}
	return nil
}

func contractFindByToken(ctx context.Context, fx *paymentsFixture) error {
	since := fx.Now().Add(-time.Hour)
	for _, sh := range fx.Shapes {
		made, nl, err := fx.made(ctx, sh)
		if err != nil {
			return err
		}
		got, found, _, err := fx.Provider.FindPaymentLink(ctx, fx.Creds, fx.Client, nl, since)
		if err != nil || !found || got.ObjectID != made.ObjectID || got.URL != made.URL {
			return failf("%s: lookup of the token = %+v, %v, %v, want %+v", sh.Name, got, found, err, made)
		}
		// The lookup makes nothing.
		before := fx.Objects()
		if _, _, _, err := fx.Provider.FindPaymentLink(ctx, fx.Creds, fx.Client, nl, since); err != nil || fx.Objects() != before {
			return failf("%s: a lookup changed the provider's objects: %v", sh.Name, err)
		}
		// A token nothing was made under is not found: no error, no object, and not "expired".
		other := nl
		if other.Token, err = newToken(); err != nil {
			return err
		}
		got, found, expired, err := fx.Provider.FindPaymentLink(ctx, fx.Creds, fx.Client, other, since)
		if err != nil || found || expired || got != (Link{}) {
			return failf("%s: lookup of another token = %+v, %v, %v, %v", sh.Name, got, found, expired, err)
		}
	}
	return nil
}

func contractFindTerms(ctx context.Context, fx *paymentsFixture) error {
	since := fx.Now().Add(-time.Hour)
	for _, sh := range fx.Shapes {
		_, nl, err := fx.made(ctx, sh)
		if err != nil {
			return err
		}
		for what, change := range map[string]func(*NewLink){
			"another amount":   func(l *NewLink) { l.AmountMinor++ },
			"another currency": func(l *NewLink) { l.Currency = "USD" },
		} {
			l := nl
			change(&l)
			got, found, _, err := fx.Provider.FindPaymentLink(ctx, fx.Creds, fx.Client, l, since)
			if !isCall(err, CallRefused) || found || got != (Link{}) {
				return failf("%s: a lookup of %s adopted %+v, %v, %v, want a refusal", sh.Name, what, got, found, err)
			}
		}
	}
	return nil
}

func contractLinkState(ctx context.Context, fx *paymentsFixture) error {
	for _, sh := range fx.Shapes {
		hosted := !sh.Pages
		l, _, err := fx.made(ctx, sh)
		if err != nil {
			return err
		}
		if st, err := fx.Provider.LinkState(ctx, fx.Creds, fx.Client, hosted, l.ObjectID); err != nil || st != LinkOpen {
			return failf("%s: a new link is %v, %v, want open", sh.Name, st, err)
		}
		if _, err := fx.Provider.LinkState(ctx, fx.Creds, fx.Client, hosted, "unknown-object"); err == nil {
			return failf("%s: the state of an object that doesn't exist was read", sh.Name)
		} else if _, ok := IsCall(err); !ok {
			return failf("%s: the state of an object that doesn't exist: %v, want a *CallError", sh.Name, err)
		}
		if _, err := fx.Pay(l, sh); err != nil {
			return err
		}
		if st, err := fx.Provider.LinkState(ctx, fx.Creds, fx.Client, hosted, l.ObjectID); err != nil || st != LinkSettled {
			return failf("%s: a paid link is %v, %v, want settled", sh.Name, st, err)
		}
		if sh.CanExpire && fx.Expire != nil {
			e, _, err := fx.made(ctx, sh)
			if err != nil {
				return err
			}
			if _, err := fx.Expire(e, sh); err != nil {
				return err
			}
			if st, err := fx.Provider.LinkState(ctx, fx.Creds, fx.Client, hosted, e.ObjectID); err != nil || st != LinkExpired {
				return failf("%s: an expired link is %v, %v, want expired", sh.Name, st, err)
			}
		}
	}
	return nil
}

// ----- webhooks -----

// anyDelivery is a genuine delivery of a payment: the first the provider sends for one.
func (fx *paymentsFixture) anyDelivery(ctx context.Context) (signedDelivery, error) {
	shapes := fx.carrying()
	if len(shapes) == 0 {
		return signedDelivery{}, failf("no shape of link has payments that carry the token")
	}
	l, _, err := fx.made(ctx, shapes[0])
	if err != nil {
		return signedDelivery{}, err
	}
	ds, err := fx.Pay(l, shapes[0])
	if err != nil || len(ds) == 0 {
		return signedDelivery{}, failf("paying a link: %d deliveries, %v", len(ds), err)
	}
	return ds[0], nil
}

func (fx *paymentsFixture) secrets() WebhookSecrets {
	return WebhookSecrets{Current: fx.Creds.WebhookSecret}
}

func (fx *paymentsFixture) verify(d signedDelivery, s WebhookSecrets, now time.Time) error {
	return fx.Provider.VerifyWebhook(d.Header, d.Body, s, now)
}

func contractVerifyGenuine(ctx context.Context, fx *paymentsFixture) error {
	for _, sh := range fx.carrying() {
		l, _, err := fx.made(ctx, sh)
		if err != nil {
			return err
		}
		ds, err := fx.Pay(l, sh)
		if err != nil {
			return err
		}
		for i, d := range ds {
			if err := fx.verify(d, fx.secrets(), fx.Now()); err != nil {
				return failf("%s: delivery %d of a payment, as the provider signed it, doesn't verify: %v", sh.Name, i, err)
			}
		}
	}
	return nil
}

func contractVerifyForged(ctx context.Context, fx *paymentsFixture) error {
	d, err := fx.anyDelivery(ctx)
	if err != nil {
		return err
	}
	now := fx.Now()
	forged := map[string]signedDelivery{
		"another secret": {Header: fx.Sign(d.Body, fx.OtherSecret, now), Body: d.Body},
		"no signature":   {Header: http.Header{}, Body: d.Body},
		"an empty one":   {Header: http.Header{"X-Nothing": {"1"}}, Body: d.Body},
		"a tampered body": {Header: d.Header, Body: func() []byte {
			b := slices.Clone(d.Body)
			b[len(b)/2] ^= 1
			return b
		}()},
		"an appended byte": {Header: d.Header, Body: append(slices.Clone(d.Body), ' ')},
		"another body":     {Header: d.Header, Body: []byte(`{}`)},
		"an empty body":    {Header: d.Header, Body: nil},
	}
	for name, f := range forged {
		if err := fx.verify(f, fx.secrets(), now); err == nil {
			return failf("a delivery with %s verifies", name)
		}
	}
	// No secret at all verifies nothing, a delivery signed with an empty secret included.
	if err := fx.verify(d, WebhookSecrets{}, now); err == nil {
		return failf("a delivery verifies against no secret")
	}
	if err := fx.verify(signedDelivery{Header: fx.Sign(d.Body, "", now), Body: d.Body}, WebhookSecrets{}, now); err == nil {
		return failf("a delivery signed with an empty secret verifies against no secret")
	}
	return nil
}

func contractVerifyTolerance(ctx context.Context, fx *paymentsFixture) error {
	d, err := fx.anyDelivery(ctx)
	if err != nil {
		return err
	}
	now := fx.Now()
	at := func(delta time.Duration) signedDelivery {
		return signedDelivery{Header: fx.Sign(d.Body, fx.Creds.WebhookSecret.Reveal(), now.Add(delta)), Body: d.Body}
	}
	for name, delta := range map[string]time.Duration{
		"just inside, in the past":   -(fx.Tolerance - time.Minute),
		"just inside, in the future": fx.Tolerance - time.Minute,
		"now":                        0,
	} {
		if err := fx.verify(at(delta), fx.secrets(), now); err != nil {
			return failf("a delivery signed %s doesn't verify: %v", name, err)
		}
	}
	for name, delta := range map[string]time.Duration{
		"just outside, in the past":   -(fx.Tolerance + time.Minute),
		"just outside, in the future": fx.Tolerance + time.Minute,
		"a day ago":                   -24 * time.Hour,
		"a year ahead":                365 * 24 * time.Hour,
	} {
		if err := fx.verify(at(delta), fx.secrets(), now); err == nil {
			return failf("a delivery signed %s verifies: the signature is right and the time isn't", name)
		}
	}
	// A recorded delivery replayed later is refused by the window, though its signature never stops being right.
	if err := fx.verify(d, fx.secrets(), now.Add(fx.Tolerance+time.Minute)); err == nil {
		return failf("a recorded delivery verifies after the window")
	}
	return nil
}

func contractVerifyRotation(ctx context.Context, fx *paymentsFixture) error {
	d, err := fx.anyDelivery(ctx)
	if err != nil {
		return err
	}
	now := fx.Now()
	other := Secret(fx.OtherSecret)
	// A new secret was set and the replaced one is kept for the overlap: deliveries signed with either verify.
	if err := fx.verify(d, WebhookSecrets{Current: other, Previous: fx.Creds.WebhookSecret}, now); err != nil {
		return failf("a delivery signed with the previous secret doesn't verify during the overlap: %v", err)
	}
	if err := fx.verify(d, WebhookSecrets{Current: fx.Creds.WebhookSecret, Previous: other}, now); err != nil {
		return failf("a delivery signed with the current secret doesn't verify during the overlap: %v", err)
	}
	// After the overlap only the new one does.
	if err := fx.verify(d, WebhookSecrets{Current: other}, now); err == nil {
		return failf("a delivery signed with a replaced secret verifies after the overlap")
	}
	return nil
}

func contractVerifyQuotesNothing(ctx context.Context, fx *paymentsFixture) error {
	d, err := fx.anyDelivery(ctx)
	if err != nil {
		return err
	}
	now := fx.Now()
	secrets := []string{fx.Creds.WebhookSecret.Reveal(), fx.OtherSecret}
	for name, f := range map[string]struct {
		d signedDelivery
		s WebhookSecrets
	}{
		"a wrong secret": {signedDelivery{Header: fx.Sign(d.Body, fx.OtherSecret, now), Body: d.Body}, fx.secrets()},
		"none":           {d, WebhookSecrets{}},
		"no header":      {signedDelivery{Header: http.Header{}, Body: d.Body}, fx.secrets()},
		"a stale one":    {signedDelivery{Header: fx.Sign(d.Body, secrets[0], now.Add(-24*time.Hour)), Body: d.Body}, fx.secrets()},
	} {
		err := fx.verify(f.d, f.s, now)
		if err == nil {
			return failf("%s verified", name)
		}
		for _, s := range secrets {
			if s != "" && strings.Contains(err.Error(), s) {
				return failf("the error of %s quotes a secret: %v", name, err)
			}
		}
		if strings.Contains(err.Error(), string(d.Body[:min(len(d.Body), 40)])) {
			return failf("the error of %s quotes the body: %v", name, err)
		}
	}
	return nil
}

// ----- events -----

var (
	contractTokenRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
	contractOpaque  = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,255}$`)
)

// normalized reads every delivery and returns the events it makes, in order, with a check that each
// delivery's verification and its normalization agree on being repeatable.
func (fx *paymentsFixture) normalized(ds []signedDelivery) ([]Event, error) {
	var out []Event
	for i, d := range ds {
		ev, ok := fx.Provider.Normalize(d.Body)
		again, ok2 := fx.Provider.Normalize(d.Body)
		if ok != ok2 || ev != again {
			return nil, failf("delivery %d is read in two ways: %+v then %+v", i, ev, again)
		}
		if ok {
			out = append(out, ev)
		}
	}
	return out, nil
}

// oneOf returns the only event of the kind among evs, and an error if there are none or several: a
// payment is told once, however many events the provider sends for it.
func oneOf(evs []Event, kind string) (Event, error) {
	var found []Event
	for _, e := range evs {
		if e.Kind == kind {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		return Event{}, failf("%d events of kind %s among %d, want exactly one: %+v", len(found), kind, len(evs), evs)
	}
	return found[0], nil
}

// closedEvent says whether an event holds only what Normalize may keep: identifiers of the provider's
// and ours, an amount and a currency, never free text, and none of what a payer typed.
func (fx *paymentsFixture) closedEvent(e Event) error {
	for name, v := range map[string]string{"id": e.ID, "object": e.ObjectID, "payment": e.PaymentID} {
		if v != "" && !contractOpaque.MatchString(v) {
			return failf("the event's %s %q isn't an identifier", name, v)
		}
	}
	if e.ID == "" || e.ObjectID == "" {
		return failf("an event with no id or no object: %+v", e)
	}
	if e.RefToken != "" && !contractTokenRe.MatchString(e.RefToken) {
		return failf("the event's token %q isn't one the server issues", e.RefToken)
	}
	if len(e.Currency) != 3 || e.Currency != strings.ToUpper(e.Currency) {
		return failf("the event's currency %q isn't an upper case code", e.Currency)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	for _, w := range fx.PayerWords {
		if strings.Contains(string(raw), w) {
			return failf("the payer's %q reached the event: %s", w, raw)
		}
	}
	return nil
}

func contractNormalizePaid(ctx context.Context, fx *paymentsFixture) error {
	for _, sh := range fx.carrying() {
		l, nl, err := fx.made(ctx, sh)
		if err != nil {
			return err
		}
		ds, err := fx.Pay(l, sh)
		if err != nil {
			return err
		}
		// What the payer typed is in what the provider sends (or the check that it doesn't come out proves nothing).
		for _, w := range fx.PayerWords {
			if !slices.ContainsFunc(ds, func(d signedDelivery) bool { return strings.Contains(string(d.Body), w) }) {
				return failf("%s: the payer's %q isn't in the provider's deliveries of a payment: the fixture proves nothing", sh.Name, w)
			}
		}
		evs, err := fx.normalized(ds)
		if err != nil {
			return err
		}
		// Several events are sent for one payment; one of them is the payment.
		paid, err := oneOf(evs, EventPaymentSucceeded)
		if err != nil {
			return failf("%s: %w", sh.Name, err)
		}
		for _, e := range evs {
			if e.Kind != EventPaymentSucceeded {
				return failf("%s: a payment also made %+v", sh.Name, e)
			}
		}
		if paid.RefToken != nl.Token || paid.AmountMinor != nl.AmountMinor || paid.Currency != nl.Currency || paid.Livemode {
			return failf("%s: the event %+v isn't the payment of %+v in test mode", sh.Name, paid, nl)
		}
		if d := paid.OccurredAt.Sub(fx.Now()); d > fx.Tolerance || d < -fx.Tolerance {
			return failf("%s: the event happened at %v, now is %v", sh.Name, paid.OccurredAt, fx.Now())
		}
		if err := fx.closedEvent(paid); err != nil {
			return failf("%s: %w", sh.Name, err)
		}
	}
	return nil
}

func contractNormalizeExpired(ctx context.Context, fx *paymentsFixture) error {
	if fx.Expire == nil {
		return nil
	}
	for _, sh := range fx.carrying() {
		if !sh.CanExpire {
			continue
		}
		l, nl, err := fx.made(ctx, sh)
		if err != nil {
			return err
		}
		ds, err := fx.Expire(l, sh)
		if err != nil {
			return err
		}
		for i, d := range ds {
			if err := fx.verify(d, fx.secrets(), fx.Now()); err != nil {
				return failf("%s: delivery %d of an expiry doesn't verify: %w", sh.Name, i, err)
			}
		}
		evs, err := fx.normalized(ds)
		if err != nil {
			return err
		}
		ev, err := oneOf(evs, EventPaymentExpired)
		if err != nil {
			return failf("%s: %w", sh.Name, err)
		}
		if len(evs) != 1 || ev.RefToken != nl.Token || ev.AmountMinor != nl.AmountMinor || ev.Currency != nl.Currency {
			return failf("%s: the expiry is %+v of %+v", sh.Name, evs, nl)
		}
		if err := fx.closedEvent(ev); err != nil {
			return failf("%s: %w", sh.Name, err)
		}
	}
	return nil
}

func contractNormalizeRefund(ctx context.Context, fx *paymentsFixture) error {
	if fx.Refund == nil {
		return nil
	}
	for _, sh := range fx.carrying() {
		l, nl, err := fx.made(ctx, sh)
		if err != nil {
			return err
		}
		paidDeliveries, err := fx.Pay(l, sh)
		if err != nil {
			return err
		}
		paidEvents, err := fx.normalized(paidDeliveries)
		if err != nil {
			return err
		}
		paid, err := oneOf(paidEvents, EventPaymentSucceeded)
		if err != nil {
			return failf("%s: %w", sh.Name, err)
		}
		if paid.PaymentID == "" {
			return failf("%s: the payment's event has no payment id, which is how a refund finds the ref", sh.Name)
		}
		part := nl.AmountMinor / 3
		// The part comes first and the rest after it: the rest is a refund of what is left, which
		// only exists once the part has been refunded, so these are steps in a fixed order.
		for _, step := range []struct {
			name        string
			minor, want int64
		}{
			{"a part", part, part},
			{"the rest", 0, nl.AmountMinor},
		} {
			name, want := step.name, step.want
			ds, err := fx.Refund(l, sh, step.minor)
			if err != nil {
				return err
			}
			for i, d := range ds {
				if err := fx.verify(d, fx.secrets(), fx.Now()); err != nil {
					return failf("%s: delivery %d of a refund doesn't verify: %w", sh.Name, i, err)
				}
			}
			evs, err := fx.normalized(ds)
			if err != nil {
				return err
			}
			ev, err := oneOf(evs, EventPaymentRefunded)
			if err != nil {
				return failf("%s, %s: %w", sh.Name, name, err)
			}
			// What has been refunded so far, in the payment's currency, mapped to the payment by its id.
			if ev.AmountMinor != want || ev.Currency != nl.Currency || ev.PaymentID != paid.PaymentID {
				return failf("%s, %s: the refund is %+v, want %d %s of payment %s", sh.Name, name, ev, want, nl.Currency, paid.PaymentID)
			}
			if err := fx.closedEvent(ev); err != nil {
				return failf("%s: %w", sh.Name, err)
			}
		}
	}
	return nil
}

func contractNormalizeIgnored(_ context.Context, fx *paymentsFixture) error {
	for name, d := range map[string]signedDelivery{"a type it doesn't act on": fx.Unacted(), "a payment that isn't ours": fx.Foreign()} {
		if err := fx.verify(d, fx.secrets(), fx.Now()); err != nil {
			return failf("the delivery of %s doesn't verify, so it proves nothing: %w", name, err)
		}
		if ev, ok := fx.Provider.Normalize(d.Body); ok {
			return failf("%s is an event: %+v", name, ev)
		}
	}
	return nil
}

func contractNormalizeGarbage(_ context.Context, fx *paymentsFixture) error {
	huge := []byte(`{"id":"` + strings.Repeat("a", 300<<10) + `"}`)
	for name, body := range map[string][]byte{
		"nothing": nil, "an empty object": []byte(`{}`), "null": []byte(`null`), "a list": []byte(`[]`), "text": []byte(`not json`),
		"a number for a type": []byte(`{"type": 5, "data": 1}`), "a huge id": huge, "a string": []byte(`"event"`),
		"nested nulls": []byte(`{"id": null, "type": null, "data": {"object": null}}`),
	} {
		if ev, ok := fx.Provider.Normalize(body); ok {
			return failf("%s is an event: %+v", name, ev)
		}
	}
	return nil
}
