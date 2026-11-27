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

type contractProvider interface {
	PaymentProvider
	Receiver
}

type linkShape struct {
	Name                string
	Pages               bool
	PaymentCarriesToken bool
	CanExpire           bool
}

type signedDelivery struct {
	Header http.Header
	Body   []byte
}

type paymentsFixture struct {
	Name         string
	Provider     contractProvider
	Client       *Client
	Creds        Credentials
	Shapes       []linkShape
	Now          func() time.Time
	Later        func(d time.Duration)
	Tolerance    time.Duration
	Ahead        time.Duration
	URL          string
	OtherSecret  string
	PayerHosts   []string
	RefusalWords []string
	PayerWords   []string
	Objects      func() int
	Pay          func(l Link, sh linkShape) ([]signedDelivery, error)
	Expire       func(l Link, sh linkShape) ([]signedDelivery, error)
	Refund       func(l Link, sh linkShape, minor int64) ([]signedDelivery, error)
	Unacted      func() signedDelivery
	Foreign      func() signedDelivery
	Sign         func(body []byte, secret string, at time.Time) signedDelivery
}

func (fx *paymentsFixture) ahead() time.Duration {
	if fx.Ahead > 0 {
		return fx.Ahead
	}
	return fx.Tolerance
}

type contractCase struct {
	name string
	run  func(ctx context.Context, fx *paymentsFixture) error
}

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
	{"a payment read back is dated when it was made, as its webhook event is, not when it was read", contractReadPaidAt},
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
		if !slices.Contains(fx.PayerHosts, u.Hostname()) {
			return failf("%s: the address %q isn't on the provider's own hosts %v", sh.Name, first.URL, fx.PayerHosts)
		}
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
		if len(fx.RefusalWords) == 0 {
			return failf("the fixture names none of the words of the provider's refusal")
		}
		for _, w := range fx.RefusalWords {
			if strings.Contains(err.Error(), w) {
				return failf("%s: the failure quotes the provider's words %q: %v", sh.Name, w, err)
			}
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
		got, found, expired, err := fx.Provider.FindPaymentLink(ctx, fx.Creds, fx.Client, nl, since)
		if err != nil || !found || expired || got.ObjectID != made.ObjectID || got.URL != made.URL {
			return failf("%s: lookup of the token = %+v, %v, expired %v, %v, want %+v", sh.Name, got, found, expired, err, made)
		}
		before := fx.Objects()
		if _, _, _, err := fx.Provider.FindPaymentLink(ctx, fx.Creds, fx.Client, nl, since); err != nil || fx.Objects() != before {
			return failf("%s: a lookup changed the provider's objects: %v", sh.Name, err)
		}
		other := nl
		if other.Token, err = newToken(); err != nil {
			return err
		}
		got, found, expired, err = fx.Provider.FindPaymentLink(ctx, fx.Creds, fx.Client, other, since)
		if err != nil || found || expired || got != (Link{}) {
			return failf("%s: lookup of another token = %+v, %v, %v, %v", sh.Name, got, found, expired, err)
		}
	}
	return nil
}

func contractFindTerms(ctx context.Context, fx *paymentsFixture) error {
	since := fx.Now().Add(-time.Hour)
	for _, sh := range fx.Shapes {
		made, nl, err := fx.made(ctx, sh)
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
			if !isCall(err, CallRefused) || found || got.URL != "" || got.ObjectID != made.ObjectID {
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
	return fx.with(fx.Creds.WebhookSecret, "")
}

func (fx *paymentsFixture) with(current, previous Secret) WebhookSecrets {
	return WebhookSecrets{Current: current, Previous: previous, URL: fx.URL}
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
		"another secret": fx.Sign(d.Body, fx.OtherSecret, now),
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
	if err := fx.verify(d, fx.with("", ""), now); err == nil {
		return failf("a delivery verifies against no secret")
	}
	if err := fx.verify(fx.Sign(d.Body, "", now), fx.with("", ""), now); err == nil {
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
		return fx.Sign(d.Body, fx.Creds.WebhookSecret.Reveal(), now.Add(delta))
	}
	for name, delta := range map[string]time.Duration{
		"just inside, in the past":   -(fx.Tolerance - time.Minute),
		"just inside, in the future": fx.ahead() - time.Minute,
		"now":                        0,
	} {
		if err := fx.verify(at(delta), fx.secrets(), now); err != nil {
			return failf("a delivery signed %s doesn't verify: %v", name, err)
		}
	}
	for name, delta := range map[string]time.Duration{
		"just outside, in the past":   -(fx.Tolerance + time.Minute),
		"just outside, in the future": fx.ahead() + time.Minute,
		"a day past the window":       -(fx.Tolerance + 24*time.Hour),
		"a year ahead":                365 * 24 * time.Hour,
	} {
		if err := fx.verify(at(delta), fx.secrets(), now); err == nil {
			return failf("a delivery signed %s verifies: the signature is right and the time isn't", name)
		}
	}
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
	if err := fx.verify(d, fx.with(other, fx.Creds.WebhookSecret), now); err != nil {
		return failf("a delivery signed with the previous secret doesn't verify during the overlap: %v", err)
	}
	if err := fx.verify(d, fx.with(fx.Creds.WebhookSecret, other), now); err != nil {
		return failf("a delivery signed with the current secret doesn't verify during the overlap: %v", err)
	}
	if err := fx.verify(d, fx.with(other, ""), now); err == nil {
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
		"a wrong secret": {fx.Sign(d.Body, fx.OtherSecret, now), fx.secrets()},
		"none":           {d, fx.with("", "")},
		"no header":      {signedDelivery{Header: http.Header{}, Body: d.Body}, fx.secrets()},
		"a stale one":    {fx.Sign(d.Body, secrets[0], now.Add(-(fx.Tolerance + 24*time.Hour))), fx.secrets()},
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

var (
	contractTokenRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
	contractOpaque  = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,255}$`)
)

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
		for _, w := range fx.PayerWords {
			if !slices.ContainsFunc(ds, func(d signedDelivery) bool { return strings.Contains(string(d.Body), w) }) {
				return failf("%s: the payer's %q isn't in the provider's deliveries of a payment: the fixture proves nothing", sh.Name, w)
			}
		}
		evs, err := fx.normalized(ds)
		if err != nil {
			return err
		}
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

func (fx *paymentsFixture) poller() (PaymentPoller, bool, error) {
	p, ok := fx.Provider.(PaymentPoller)
	if ok && fx.Later == nil {
		return nil, false, failf("the adapter polls, and the fixture can't move the provider's clock (Later)")
	}
	return p, ok, nil
}

const readGap = 40 * time.Second

func contractReadPaidAt(ctx context.Context, fx *paymentsFixture) error {
	poller, ok, err := fx.poller()
	if !ok {
		return err
	}
	for _, sh := range fx.Shapes {
		l, _, err := fx.made(ctx, sh)
		if err != nil {
			return err
		}
		fx.Later(time.Minute)
		paidAt := fx.Now().Truncate(time.Second)
		ds, err := fx.Pay(l, sh)
		if err != nil {
			return err
		}
		fx.Later(readGap)
		obs, err := poller.ReadPayment(ctx, fx.Creds, fx.Client, !sh.Pages, l.ObjectID)
		if err != nil {
			return failf("%s: reading a paid link: %w", sh.Name, err)
		}
		if len(obs.Payments) != 1 {
			return failf("%s: a read of a link paid once found %d payments", sh.Name, len(obs.Payments))
		}
		got := obs.Payments[0].PaidAt
		if got.IsZero() {
			return failf("%s: the read says no time of the payment, though the provider says one", sh.Name)
		}
		if !got.Equal(paidAt) {
			return failf("%s: the payment made at %v and read at %v is dated %v", sh.Name, paidAt, fx.Now(), got)
		}
		if !sh.PaymentCarriesToken {
			continue
		}
		evs, err := fx.normalized(ds)
		if err != nil {
			return err
		}
		paid, err := oneOf(evs, EventPaymentSucceeded)
		if err != nil {
			return failf("%s: %w", sh.Name, err)
		}
		if !paid.OccurredAt.Equal(got) {
			return failf("%s: the webhook's event is dated %v, the read %v", sh.Name, paid.OccurredAt, got)
		}
	}
	return nil
}
